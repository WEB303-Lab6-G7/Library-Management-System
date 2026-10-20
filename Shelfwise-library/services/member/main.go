// Member Service — owns library members and their membership status.
// Publishes: MemberRegistered, MemberSuspended, MemberReactivated
// Consumes:  FineIssued, FinePaid (to suspend / reactivate members)
//
// It also owns credentials: passwords are stored only as salted PBKDF2 hashes.
// The gateway calls /auth/login to check a password and then issues the session.
package main

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"library/pkg/events"
	"library/pkg/store"
	"library/pkg/svc"
)

const service = "member"

var (
	db            *sql.DB
	fineThreshold float64
)

var schema = []string{
	`CREATE TABLE IF NOT EXISTS members (
		id                BIGSERIAL PRIMARY KEY,
		name              TEXT NOT NULL,
		email             TEXT NOT NULL UNIQUE,
		status            TEXT NOT NULL DEFAULT 'active', -- active | suspended
		borrow_limit      INT  NOT NULL DEFAULT 3,
		outstanding_fines NUMERIC(10,2) NOT NULL DEFAULT 0,
		status_reason     TEXT NOT NULL DEFAULT '',
		created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	// Added for authentication (safe to run on an existing database).
	`ALTER TABLE members ADD COLUMN IF NOT EXISTS password_hash TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE members ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'member'`, // member | librarian
}

type Member struct {
	ID               int64   `json:"id"`
	Name             string  `json:"name"`
	Email            string  `json:"email"`
	Status           string  `json:"status"`
	BorrowLimit      int     `json:"borrowLimit"`
	OutstandingFines float64 `json:"outstandingFines"`
	StatusReason     string  `json:"statusReason"`
	Role             string  `json:"role"`
}

func main() {
	fineThreshold = float64(svc.EnvInt("FINE_SUSPEND_THRESHOLD", 50))
	var err error
	db, err = store.Open(svc.Env("DATABASE_URL", "postgres://library:library@localhost:5432/member_db?sslmode=disable"), schema...)
	if err != nil {
		log.Fatal(err)
	}
	broker, err := events.Connect(svc.Env("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/"), service)
	if err != nil {
		log.Fatal(err)
	}
	go store.RunOutboxRelay(db, broker)
	seedLibrarian()

	if err := broker.Subscribe("member.events",
		[]string{events.FineIssued, events.FinePaid},
		store.Idempotent(db, handleEvent)); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /members", listMembers)
	mux.HandleFunc("POST /members", registerMember)
	mux.HandleFunc("GET /members/{id}", getMember)
	mux.HandleFunc("POST /auth/login", login)
	svc.Serve(service, ":"+svc.Env("PORT", "8082"), mux)
}

// ---------- Event handlers ----------

func handleEvent(ctx context.Context, tx *sql.Tx, env events.Envelope) error {
	var memberID int64
	var total float64
	switch env.EventType {
	case events.FineIssued:
		var d events.FineIssuedData
		if err := env.Decode(&d); err != nil {
			return err
		}
		memberID, total = d.MemberID, d.TotalOutstanding
	case events.FinePaid:
		var d events.FinePaidData
		if err := env.Decode(&d); err != nil {
			return err
		}
		memberID, total = d.MemberID, d.TotalOutstanding
	default:
		return nil
	}

	m, err := scanMember(tx.QueryRowContext(ctx, memberQuery+` WHERE id=$1 FOR UPDATE`, memberID))
	if err == sql.ErrNoRows {
		return fmt.Errorf("member %d not found", memberID) // retried, then dead-lettered
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE members SET outstanding_fines=$2 WHERE id=$1`, memberID, total); err != nil {
		return err
	}
	log.Printf("[member] member %d outstanding fines now Nu.%.2f", memberID, total)

	// Event chaining: a fine can change membership status, which is itself an event.
	switch {
	case m.Status == "active" && total > fineThreshold:
		reason := fmt.Sprintf("Outstanding fines Nu.%.2f exceed the limit of Nu.%.0f", total, fineThreshold)
		if _, err := tx.ExecContext(ctx, `UPDATE members SET status='suspended', status_reason=$2 WHERE id=$1`, memberID, reason); err != nil {
			return err
		}
		_, err = store.Enqueue(ctx, tx, service, events.MemberSuspended, events.MemberStatusData{
			MemberID: m.ID, Name: m.Name, Email: m.Email, Status: "suspended", Reason: reason, OutstandingFines: total})
		log.Printf("[member] member %d SUSPENDED", memberID)
		return err
	case m.Status == "suspended" && total <= fineThreshold:
		reason := "Fines paid"
		if _, err := tx.ExecContext(ctx, `UPDATE members SET status='active', status_reason=$2 WHERE id=$1`, memberID, reason); err != nil {
			return err
		}
		_, err = store.Enqueue(ctx, tx, service, events.MemberReactivated, events.MemberStatusData{
			MemberID: m.ID, Name: m.Name, Email: m.Email, Status: "active", Reason: reason, OutstandingFines: total})
		log.Printf("[member] member %d REACTIVATED", memberID)
		return err
	}
	return nil
}

// ---------- HTTP ----------

const memberQuery = `SELECT id, name, email, status, borrow_limit, outstanding_fines::float8, status_reason, role FROM members`

func scanMember(s interface{ Scan(...any) error }) (Member, error) {
	var m Member
	err := s.Scan(&m.ID, &m.Name, &m.Email, &m.Status, &m.BorrowLimit, &m.OutstandingFines, &m.StatusReason, &m.Role)
	return m, err
}

// ---------- Passwords ----------

const pbkdf2Iterations = 120_000

// hashPassword returns "pbkdf2-sha256$<iterations>$<salt>$<hash>".
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// seedLibrarian creates the first staff account so someone can manage the library.
func seedLibrarian() {
	email := strings.ToLower(svc.Env("LIBRARIAN_EMAIL", "librarian@library.bt"))
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM members WHERE role='librarian')`).Scan(&exists); err != nil || exists {
		return
	}
	hash, err := hashPassword(svc.Env("LIBRARIAN_PASSWORD", "librarian123"))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO members (name, email, password_hash, role) VALUES ($1,$2,$3,'librarian')
		ON CONFLICT (email) DO UPDATE SET role='librarian', password_hash=$3`,
		svc.Env("LIBRARIAN_NAME", "Head Librarian"), email, hash); err != nil {
		log.Fatal(err)
	}
	log.Printf("[member] created librarian account %s", email)
}

func login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := svc.Decode(r, &in); err != nil || in.Email == "" || in.Password == "" {
		svc.Error(w, 400, "email and password are required")
		return
	}
	var hash string
	m, err := scanMember(db.QueryRowContext(r.Context(), memberQuery+` WHERE email=$1`, strings.ToLower(strings.TrimSpace(in.Email))))
	if err == nil {
		err = db.QueryRowContext(r.Context(), `SELECT password_hash FROM members WHERE id=$1`, m.ID).Scan(&hash)
	}
	if err != nil || !checkPassword(hash, in.Password) {
		svc.Error(w, 401, "incorrect email or password")
		return
	}
	svc.JSON(w, 200, m)
}

func listMembers(w http.ResponseWriter, r *http.Request) {
	rows, err := db.QueryContext(r.Context(), memberQuery+` WHERE role='member' ORDER BY id`)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	list := []Member{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			svc.Error(w, 500, "%v", err)
			return
		}
		list = append(list, m)
	}
	svc.JSON(w, 200, list)
}

func getMember(w http.ResponseWriter, r *http.Request) {
	id, err := svc.PathID(r, "id")
	if err != nil {
		svc.Error(w, 400, "invalid member id")
		return
	}
	m, err := scanMember(db.QueryRowContext(r.Context(), memberQuery+` WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		svc.Error(w, 404, "member %d not found", id)
		return
	} else if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	svc.JSON(w, 200, m)
}

func registerMember(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Email       string `json:"email"`
		Password    string `json:"password"`
		BorrowLimit int    `json:"borrowLimit"`
	}
	if err := svc.Decode(r, &in); err != nil || strings.TrimSpace(in.Name) == "" || !strings.Contains(in.Email, "@") {
		svc.Error(w, 400, "a name and a valid email are required")
		return
	}
	if len(in.Password) < 6 {
		svc.Error(w, 400, "the password must be at least 6 characters")
		return
	}
	in.Name, in.Email = strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Email))
	hash, err := hashPassword(in.Password)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if in.BorrowLimit < 1 {
		in.BorrowLimit = 3
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	var id int64
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO members (name, email, borrow_limit, password_hash) VALUES ($1,$2,$3,$4) RETURNING id`,
		in.Name, in.Email, in.BorrowLimit, hash).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			svc.Error(w, 409, "an account with %s already exists", in.Email)
			return
		}
		svc.Error(w, 500, "%v", err)
		return
	}
	if _, err := store.Enqueue(r.Context(), tx, service, events.MemberRegistered, events.MemberRegisteredData{
		MemberID: id, Name: in.Name, Email: strings.ToLower(in.Email), BorrowLimit: in.BorrowLimit}); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	svc.JSON(w, 201, Member{ID: id, Name: in.Name, Email: in.Email, Status: "active", BorrowLimit: in.BorrowLimit, Role: "member"})
}
