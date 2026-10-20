// Notification Service — turns events into messages for members, and keeps an
// audit log of every event seen on the bus.
// Publishes: nothing
// Consumes:  every event ("#" binding)
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"library/pkg/events"
	"library/pkg/store"
	"library/pkg/svc"
)

const service = "notification"

var db *sql.DB

var schema = []string{
	`CREATE TABLE IF NOT EXISTS event_log (
		event_id    TEXT PRIMARY KEY,
		event_type  TEXT NOT NULL,
		source      TEXT NOT NULL,
		occurred_at TIMESTAMPTZ NOT NULL,
		received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		envelope    JSONB NOT NULL,
		duplicates  INT NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS notifications (
		id         BIGSERIAL PRIMARY KEY,
		member_id  BIGINT,
		recipient  TEXT NOT NULL,
		subject    TEXT NOT NULL,
		message    TEXT NOT NULL,
		event_type TEXT NOT NULL,
		event_id   TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
}

func main() {
	var err error
	db, err = store.Open(svc.Env("DATABASE_URL", "postgres://library:library@localhost:5432/notification_db?sslmode=disable"), schema...)
	if err != nil {
		log.Fatal(err)
	}
	broker, err := events.Connect(svc.Env("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/"), service)
	if err != nil {
		log.Fatal(err)
	}
	if err := broker.Subscribe("notification.events", []string{"#"}, handleEvent); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /notifications", listNotifications)
	mux.HandleFunc("GET /events", listEvents)
	svc.Serve(service, ":"+svc.Env("PORT", "8086"), mux)
}

// handleEvent is idempotent through the event_log primary key: a repeated eventId
// only increments the duplicate counter and sends nothing.
func handleEvent(ctx context.Context, env events.Envelope) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	raw, _ := json.Marshal(env)
	var inserted bool
	err = tx.QueryRowContext(ctx, `
		INSERT INTO event_log (event_id, event_type, source, occurred_at, envelope) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (event_id) DO UPDATE SET duplicates = event_log.duplicates + 1
		RETURNING (xmax = 0)`, env.EventID, env.EventType, env.Source, env.OccurredAt, raw).Scan(&inserted)
	if err != nil {
		return err
	}
	if !inserted {
		log.Printf("[idempotency] duplicate %s id=%s — no notification sent", env.EventType, env.EventID)
		return tx.Commit()
	}

	n, err := compose(env)
	if err != nil {
		return err
	}
	if n != nil {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notifications (member_id, recipient, subject, message, event_type, event_id)
			VALUES ($1,$2,$3,$4,$5,$6)`, n.memberID, n.to, n.subject, n.body, env.EventType, env.EventID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if n != nil {
		// Simulated email delivery.
		log.Printf("[email] to=%s subject=%q", n.to, n.subject)
	} else {
		log.Printf("[notification] logged %s id=%s", env.EventType, env.EventID)
	}
	return nil
}

type note struct {
	memberID      int64
	to            string
	subject, body string
}

func day(t time.Time) string { return t.Format("Mon 2 Jan 2006") }

func compose(env events.Envelope) (*note, error) {
	switch env.EventType {
	case events.MemberRegistered:
		var d events.MemberRegisteredData
		if err := env.Decode(&d); err != nil {
			return nil, err
		}
		return &note{d.MemberID, d.Email, "Welcome to the library",
			fmt.Sprintf("Hi %s, your membership is active. You can borrow up to %d books at a time.", d.Name, d.BorrowLimit)}, nil
	case events.BookBorrowed:
		var d events.BookBorrowedData
		if err := env.Decode(&d); err != nil {
			return nil, err
		}
		return &note{d.MemberID, d.Email, "Borrowing receipt: " + d.Title,
			fmt.Sprintf("Hi %s, you borrowed %q (copy #%d). Please return it by %s.", d.MemberName, d.Title, d.CopyID, day(d.DueDate))}, nil
	case events.BookReturned:
		var d events.BookReturnedData
		if err := env.Decode(&d); err != nil {
			return nil, err
		}
		msg := fmt.Sprintf("Hi %s, thanks for returning %q.", d.MemberName, d.Title)
		if d.DaysLate > 0 {
			msg += fmt.Sprintf(" It was %d day(s) late, so a fine will follow.", d.DaysLate)
		}
		return &note{d.MemberID, d.Email, "Return confirmed: " + d.Title, msg}, nil
	case events.BookOverdue:
		var d events.BookOverdueData
		if err := env.Decode(&d); err != nil {
			return nil, err
		}
		return &note{d.MemberID, d.Email, "Overdue: " + d.Title,
			fmt.Sprintf("Hi %s, %q was due on %s and is now %d day(s) overdue. Late fees are accruing.", d.MemberName, d.Title, day(d.DueDate), d.DaysLate)}, nil
	case events.FineIssued:
		var d events.FineIssuedData
		if err := env.Decode(&d); err != nil {
			return nil, err
		}
		return &note{d.MemberID, d.Email, fmt.Sprintf("Fine of Nu.%.2f", d.Amount),
			fmt.Sprintf("Hi %s, a fine of Nu.%.2f was charged for %q (%d day(s) late). Total outstanding: Nu.%.2f.", d.MemberName, d.Amount, d.Title, d.DaysLate, d.TotalOutstanding)}, nil
	case events.FinePaid:
		var d events.FinePaidData
		if err := env.Decode(&d); err != nil {
			return nil, err
		}
		return &note{d.MemberID, d.Email, "Payment received",
			fmt.Sprintf("Hi %s, we received Nu.%.2f. Remaining balance: Nu.%.2f.", d.MemberName, d.Amount, d.TotalOutstanding)}, nil
	case events.MemberSuspended:
		var d events.MemberStatusData
		if err := env.Decode(&d); err != nil {
			return nil, err
		}
		return &note{d.MemberID, d.Email, "Membership suspended",
			fmt.Sprintf("Hi %s, your borrowing privileges are suspended. %s.", d.Name, d.Reason)}, nil
	case events.MemberReactivated:
		var d events.MemberStatusData
		if err := env.Decode(&d); err != nil {
			return nil, err
		}
		return &note{d.MemberID, d.Email, "Membership reactivated",
			fmt.Sprintf("Hi %s, your account is active again. Happy reading!", d.Name)}, nil
	case events.HoldPlaced:
		var d events.HoldPlacedData
		if err := env.Decode(&d); err != nil {
			return nil, err
		}
		return &note{d.MemberID, d.Email, "Hold placed: " + d.Title,
			fmt.Sprintf("Hi %s, you are #%d in the queue for %q. We'll tell you when it's back.", d.MemberName, d.Position, d.Title)}, nil
	case events.HoldReady:
		var d events.HoldReadyData
		if err := env.Decode(&d); err != nil {
			return nil, err
		}
		return &note{d.MemberID, d.Email, "Ready for pickup: " + d.Title,
			fmt.Sprintf("Hi %s, %q (copy #%d) is waiting for you at the desk until %s.", d.MemberName, d.Title, d.CopyID, day(d.ExpiresAt))}, nil
	}
	return nil, nil // e.g. BookAdded: logged only
}

// ---------- HTTP ----------

func listNotifications(w http.ResponseWriter, r *http.Request) {
	q := `SELECT id, COALESCE(member_id,0), recipient, subject, message, event_type, event_id, created_at FROM notifications`
	var args []any
	if v := r.URL.Query().Get("memberId"); v != "" {
		q += ` WHERE member_id=$1`
		args = append(args, v)
	}
	rows, err := db.QueryContext(r.Context(), q+` ORDER BY id DESC LIMIT 100`, args...)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	type N struct {
		ID        int64     `json:"id"`
		MemberID  int64     `json:"memberId"`
		Recipient string    `json:"recipient"`
		Subject   string    `json:"subject"`
		Message   string    `json:"message"`
		EventType string    `json:"eventType"`
		EventID   string    `json:"eventId"`
		CreatedAt time.Time `json:"createdAt"`
	}
	list := []N{}
	for rows.Next() {
		var n N
		if err := rows.Scan(&n.ID, &n.MemberID, &n.Recipient, &n.Subject, &n.Message, &n.EventType, &n.EventID, &n.CreatedAt); err != nil {
			svc.Error(w, 500, "%v", err)
			return
		}
		list = append(list, n)
	}
	svc.JSON(w, 200, list)
}

func listEvents(w http.ResponseWriter, r *http.Request) {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 || limit > 500 {
		limit = 60
	}
	rows, err := db.QueryContext(r.Context(),
		`SELECT envelope, received_at, duplicates FROM event_log ORDER BY received_at DESC LIMIT $1`, limit)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	type E struct {
		events.Envelope
		ReceivedAt time.Time `json:"receivedAt"`
		Duplicates int       `json:"duplicates"`
	}
	list := []E{}
	for rows.Next() {
		var e E
		var raw []byte
		if err := rows.Scan(&raw, &e.ReceivedAt, &e.Duplicates); err != nil {
			svc.Error(w, 500, "%v", err)
			return
		}
		_ = json.Unmarshal(raw, &e.Envelope)
		list = append(list, e)
	}
	svc.JSON(w, 200, list)
}
