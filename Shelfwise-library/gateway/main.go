// API Gateway — the single entry point for the website.
//
//   - Serves the website (gateway/web) with clean URLs such as /catalog and /account.
//   - Authenticates users: the Member service checks the password, then the gateway
//     issues a signed, HttpOnly session cookie.
//   - Authorises every API call by role (member or librarian). A member can only see
//     and act on their own loans, holds, fines and notifications.
//   - Forwards allowed calls to the right microservice.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"library/pkg/events"
	"library/pkg/svc"
)

//go:embed web
var webFS embed.FS

var services = map[string]string{
	"catalog":      svc.Env("CATALOG_URL", "http://localhost:8081"),
	"member":       svc.Env("MEMBER_URL", "http://localhost:8082"),
	"circulation":  svc.Env("CIRCULATION_URL", "http://localhost:8083"),
	"reservation":  svc.Env("RESERVATION_URL", "http://localhost:8084"),
	"fine":         svc.Env("FINE_URL", "http://localhost:8085"),
	"notification": svc.Env("NOTIFICATION_URL", "http://localhost:8086"),
}

var (
	broker      *events.Broker
	rabbitAPI   = svc.Env("RABBITMQ_API", "http://localhost:15672")
	rabbitUser  = svc.Env("RABBITMQ_USER", "guest")
	rabbitPass  = svc.Env("RABBITMQ_PASS", "guest")
	quickClient = &http.Client{Timeout: 1500 * time.Millisecond}
	upClient    = &http.Client{Timeout: 5 * time.Second}
)

func main() {
	secret := svc.Env("SESSION_SECRET", "")
	if secret == "" {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		secret = hex.EncodeToString(b)
		log.Printf("[gateway] SESSION_SECRET not set; using a random one (sessions end when the gateway restarts)")
	}
	sessionKey = []byte(secret)

	var err error
	broker, err = events.Connect(svc.Env("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/"), "gateway")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	// Authentication
	mux.HandleFunc("POST /api/auth/register", register)
	mux.HandleFunc("POST /api/auth/login", login)
	mux.HandleFunc("POST /api/auth/logout", logout)
	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		if s := currentSession(r); s != nil {
			me(w, r, s)
			return
		}
		svc.JSON(w, 200, nil) // not signed in
	})

	// Catalogue: anyone can browse; only librarians add books
	mux.HandleFunc("GET /api/books", func(w http.ResponseWriter, r *http.Request) { forward(w, r, "catalog", "/books", nil) })
	mux.HandleFunc("GET /api/books/{id}", func(w http.ResponseWriter, r *http.Request) {
		forward(w, r, "catalog", "/books/"+r.PathValue("id"), nil)
	})
	mux.HandleFunc("POST /api/books", requireRole("librarian", func(w http.ResponseWriter, r *http.Request, s *session) {
		forward(w, r, "catalog", "/books", readBody(r))
	}))

	// Circulation
	mux.HandleFunc("GET /api/loans", requireAuth(scopedList("circulation", "/loans")))
	mux.HandleFunc("POST /api/loans", requireAuth(borrow))
	mux.HandleFunc("POST /api/loans/{id}/return", requireAuth(returnLoan))

	// Holds
	mux.HandleFunc("GET /api/holds", requireAuth(scopedList("reservation", "/holds")))
	mux.HandleFunc("POST /api/holds", requireAuth(placeHold))

	// Fines
	mux.HandleFunc("GET /api/fines", requireAuth(scopedList("fine", "/fines")))
	mux.HandleFunc("POST /api/fines/{id}/pay", requireAuth(payFine))

	// Notifications
	mux.HandleFunc("GET /api/notifications", requireAuth(scopedList("notification", "/notifications")))

	// Librarian-only views
	mux.HandleFunc("GET /api/members", requireRole("librarian", func(w http.ResponseWriter, r *http.Request, s *session) {
		forward(w, r, "member", "/members", nil)
	}))
	mux.HandleFunc("GET /api/member-view", requireRole("librarian", func(w http.ResponseWriter, r *http.Request, s *session) {
		forward(w, r, "circulation", "/member-view", nil)
	}))

	// System monitor (librarian only)
	mux.HandleFunc("GET /api/system/status", requireRole("librarian", status))
	mux.HandleFunc("GET /api/system/events", requireRole("librarian", func(w http.ResponseWriter, r *http.Request, s *session) {
		forward(w, r, "notification", "/events?"+r.URL.RawQuery, nil)
	}))
	mux.HandleFunc("POST /api/system/replay", requireRole("librarian", replay))
	mux.HandleFunc("GET /api/system/failure-mode", requireRole("librarian", func(w http.ResponseWriter, r *http.Request, s *session) {
		forward(w, r, "fine", "/admin/failure-mode", nil)
	}))
	mux.HandleFunc("POST /api/system/failure-mode", requireRole("librarian", func(w http.ResponseWriter, r *http.Request, s *session) {
		forward(w, r, "fine", "/admin/failure-mode", readBody(r))
	}))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { svc.Error(w, 404, "no such API endpoint") })
	mux.Handle("/", website())

	svc.Serve("gateway", ":"+svc.Env("PORT", "8080"), mux)
}

// ---------- Upstream calls ----------

var errUnavailable = errors.New("service unavailable")

// call sends a request to a service and returns its status and body.
func call(ctx context.Context, method, service, path string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, services[service]+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := upClient.Do(req)
	if err != nil {
		log.Printf("[gateway] %s unreachable: %v", service, err)
		return 0, nil, errUnavailable
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, out, err
}

// forward relays a call to a service back to the browser unchanged.
func forward(w http.ResponseWriter, r *http.Request, service, path string, body []byte) {
	if body == nil && r.Method == http.MethodGet && r.URL.RawQuery != "" && !strings.Contains(path, "?") {
		path += "?" + r.URL.RawQuery
	}
	status, out, err := call(r.Context(), r.Method, service, path, body)
	if err != nil {
		svc.Error(w, http.StatusServiceUnavailable, "The %s service is unavailable right now. Please try again shortly.", service)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func readBody(r *http.Request) []byte {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

// ---------- Auth endpoints ----------

func register(w http.ResponseWriter, r *http.Request) {
	status, out, err := call(r.Context(), "POST", "member", "/members", readBody(r))
	finishLogin(w, status, out, err, 201)
}

func login(w http.ResponseWriter, r *http.Request) {
	status, out, err := call(r.Context(), "POST", "member", "/auth/login", readBody(r))
	finishLogin(w, status, out, err, 200)
}

// finishLogin starts a session when the Member service accepted the credentials.
func finishLogin(w http.ResponseWriter, status int, out []byte, err error, okStatus int) {
	if err != nil {
		svc.Error(w, 503, "Sign-in is unavailable right now because the member service is down.")
		return
	}
	if status != okStatus {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(out)
		return
	}
	var u struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.Unmarshal(out, &u); err != nil || u.ID == 0 {
		svc.Error(w, 502, "unexpected response from the member service")
		return
	}
	setSession(w, &session{UID: u.ID, Name: u.Name, Email: u.Email, Role: u.Role})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func logout(w http.ResponseWriter, r *http.Request) {
	clearSession(w)
	svc.JSON(w, 200, map[string]bool{"ok": true})
}

func me(w http.ResponseWriter, r *http.Request, s *session) {
	status, out, err := call(r.Context(), "GET", "member", fmt.Sprintf("/members/%d", s.UID), nil)
	if err != nil {
		// Member service down: still return what the session knows.
		svc.JSON(w, 200, map[string]any{"id": s.UID, "name": s.Name, "email": s.Email, "role": s.Role, "status": "unknown"})
		return
	}
	if status == 404 {
		clearSession(w)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// ---------- Scoped resources ----------

// scopedList lists a resource; members only ever see their own rows.
func scopedList(service, path string) func(http.ResponseWriter, *http.Request, *session) {
	return func(w http.ResponseWriter, r *http.Request, s *session) {
		q := r.URL.Query()
		if s.Role != "librarian" {
			q.Set("memberId", strconv.FormatInt(s.UID, 10))
		}
		p := path
		if enc := q.Encode(); enc != "" {
			p += "?" + enc
		}
		forward(w, r, service, p, nil)
	}
}

// owns reports whether the signed-in member owns the item with this id in the given list.
func owns(r *http.Request, s *session, service, path, id string) (bool, error) {
	_, out, err := call(r.Context(), "GET", service, fmt.Sprintf("%s?memberId=%d", path, s.UID), nil)
	if err != nil {
		return false, err
	}
	var items []struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(out, &items)
	for _, it := range items {
		if strconv.FormatInt(it.ID, 10) == id {
			return true, nil
		}
	}
	return false, nil
}

// borrow: members borrow for themselves with the standard loan period. Librarians
// issue loans at the desk for any member and may set the period (a negative period
// back-dates the loan, which is how the demo creates overdue books).
func borrow(w http.ResponseWriter, r *http.Request, s *session) {
	var in struct {
		MemberID  int64 `json:"memberId"`
		BookID    int64 `json:"bookId"`
		DueInDays *int  `json:"dueInDays,omitempty"`
	}
	if err := json.Unmarshal(readBody(r), &in); err != nil || in.BookID == 0 {
		svc.Error(w, 400, "bookId is required")
		return
	}
	if s.Role != "librarian" {
		in.MemberID, in.DueInDays = s.UID, nil
	} else if in.MemberID == 0 {
		svc.Error(w, 400, "choose the member who is borrowing")
		return
	}
	body, _ := json.Marshal(in)
	forward(w, r, "circulation", "/loans", body)
}

func returnLoan(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.PathValue("id")
	if s.Role != "librarian" {
		ok, err := owns(r, s, "circulation", "/loans", id)
		if err != nil {
			svc.Error(w, 503, "The circulation service is unavailable right now.")
			return
		}
		if !ok {
			svc.Error(w, 404, "loan not found")
			return
		}
	}
	forward(w, r, "circulation", "/loans/"+url.PathEscape(id)+"/return", []byte("{}"))
}

func placeHold(w http.ResponseWriter, r *http.Request, s *session) {
	var in struct {
		MemberID int64 `json:"memberId"`
		BookID   int64 `json:"bookId"`
	}
	if err := json.Unmarshal(readBody(r), &in); err != nil || in.BookID == 0 {
		svc.Error(w, 400, "bookId is required")
		return
	}
	if s.Role != "librarian" {
		in.MemberID = s.UID
	} else if in.MemberID == 0 {
		svc.Error(w, 400, "choose the member placing the hold")
		return
	}
	body, _ := json.Marshal(in)
	forward(w, r, "reservation", "/holds", body)
}

func payFine(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.PathValue("id")
	if s.Role != "librarian" {
		ok, err := owns(r, s, "fine", "/fines", id)
		if err != nil {
			svc.Error(w, 503, "The fine service is unavailable right now.")
			return
		}
		if !ok {
			svc.Error(w, 404, "fine not found")
			return
		}
	}
	forward(w, r, "fine", "/fines/"+url.PathEscape(id)+"/pay", []byte("{}"))
}

// ---------- System monitor ----------

type queueInfo struct {
	Name      string `json:"name"`
	Ready     int    `json:"messages_ready"`
	Unacked   int    `json:"messages_unacknowledged"`
	Consumers int    `json:"consumers"`
}

// status reports which services are up and how many messages wait in each queue.
func status(w http.ResponseWriter, r *http.Request, _ *session) {
	health := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, base := range services {
		wg.Add(1)
		go func(name, base string) {
			defer wg.Done()
			ok := false
			if resp, err := quickClient.Get(base + "/health"); err == nil {
				ok = resp.StatusCode == 200
				resp.Body.Close()
			}
			mu.Lock()
			health[name] = ok
			mu.Unlock()
		}(name, base)
	}

	queues := []queueInfo{}
	var queueErr string
	req, _ := http.NewRequestWithContext(r.Context(), "GET", rabbitAPI+"/api/queues?columns=name,messages_ready,messages_unacknowledged,consumers", nil)
	req.SetBasicAuth(rabbitUser, rabbitPass)
	if resp, err := quickClient.Do(req); err != nil {
		queueErr = err.Error()
	} else {
		_ = json.NewDecoder(resp.Body).Decode(&queues)
		resp.Body.Close()
	}
	wg.Wait()
	svc.JSON(w, 200, map[string]any{"services": health, "queues": queues, "queueError": queueErr})
}

// replay re-publishes an event exactly as it was (same eventId), simulating the
// broker delivering it twice. Consumers must recognise and ignore it.
func replay(w http.ResponseWriter, r *http.Request, _ *session) {
	var env events.Envelope
	if err := json.Unmarshal(readBody(r), &env); err != nil || env.EventID == "" || env.EventType == "" {
		svc.Error(w, 400, "send the full event envelope (eventId, eventType, data)")
		return
	}
	if strings.ContainsAny(env.EventType, "#*") {
		svc.Error(w, 400, "invalid event type")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := broker.Publish(ctx, env); err != nil {
		svc.Error(w, 502, "publish failed: %v", err)
		return
	}
	log.Printf("[gateway] replayed %s id=%s (duplicate delivery test)", env.EventType, env.EventID)
	svc.JSON(w, 202, map[string]string{"replayed": env.EventID, "eventType": env.EventType})
}

// ---------- Website ----------

// website serves static files, and index.html for every page route so that
// URLs like /catalog or /admin/system load the single-page app.
func website() http.Handler {
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "." {
			if st, err := fs.Stat(static, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "no-cache")
				}
				files.ServeHTTP(w, r)
				return
			}
			if path.Ext(p) != "" {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, static, "index.html")
	})
}
