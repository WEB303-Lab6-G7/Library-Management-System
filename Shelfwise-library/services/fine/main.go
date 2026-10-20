// Fine Service — owns late fees and payments.
// Publishes: FineIssued, FinePaid
// Consumes:  BookOverdue, BookReturned
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"library/pkg/events"
	"library/pkg/store"
	"library/pkg/svc"
)

const service = "fine"

var (
	db         *sql.DB
	finePerDay float64
	// failureMode makes every event handler fail, to demonstrate retries and the DLQ.
	failureMode atomic.Bool
)

var schema = []string{
	`CREATE TABLE IF NOT EXISTS fines (
		id          BIGSERIAL PRIMARY KEY,
		loan_id     BIGINT NOT NULL UNIQUE, -- one fine per loan
		member_id   BIGINT NOT NULL,
		member_name TEXT NOT NULL,
		email       TEXT NOT NULL,
		title       TEXT NOT NULL,
		days_late   INT NOT NULL,
		amount      NUMERIC(10,2) NOT NULL,
		status      TEXT NOT NULL DEFAULT 'unpaid', -- unpaid | paid
		created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
		paid_at     TIMESTAMPTZ
	)`,
}

type Fine struct {
	ID         int64      `json:"id"`
	LoanID     int64      `json:"loanId"`
	MemberID   int64      `json:"memberId"`
	MemberName string     `json:"memberName"`
	Title      string     `json:"title"`
	DaysLate   int        `json:"daysLate"`
	Amount     float64    `json:"amount"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	PaidAt     *time.Time `json:"paidAt"`
}

func main() {
	finePerDay = float64(svc.EnvInt("FINE_PER_DAY", 10))
	var err error
	db, err = store.Open(svc.Env("DATABASE_URL", "postgres://library:library@localhost:5432/fine_db?sslmode=disable"), schema...)
	if err != nil {
		log.Fatal(err)
	}
	broker, err := events.Connect(svc.Env("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/"), service)
	if err != nil {
		log.Fatal(err)
	}
	go store.RunOutboxRelay(db, broker)

	if err := broker.Subscribe("fine.events",
		[]string{events.BookOverdue, events.BookReturned},
		store.Idempotent(db, handleEvent)); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /fines", listFines)
	mux.HandleFunc("POST /fines/{id}/pay", payFine)
	mux.HandleFunc("GET /admin/failure-mode", getFailureMode)
	mux.HandleFunc("POST /admin/failure-mode", setFailureMode)
	svc.Serve(service, ":"+svc.Env("PORT", "8085"), mux)
}

// ---------- Event handlers ----------

type lateLoan struct {
	loanID, memberID   int64
	name, email, title string
	daysLate           int
}

func handleEvent(ctx context.Context, tx *sql.Tx, env events.Envelope) error {
	if failureMode.Load() {
		return errors.New("simulated processing failure (failure mode is ON)")
	}
	var l lateLoan
	switch env.EventType {
	case events.BookOverdue:
		var d events.BookOverdueData
		if err := env.Decode(&d); err != nil {
			return err
		}
		l = lateLoan{d.LoanID, d.MemberID, d.MemberName, d.Email, d.Title, d.DaysLate}
	case events.BookReturned:
		var d events.BookReturnedData
		if err := env.Decode(&d); err != nil {
			return err
		}
		if d.DaysLate <= 0 {
			log.Printf("[fine] %q returned on time — no fine", d.Title)
			return nil
		}
		l = lateLoan{d.LoanID, d.MemberID, d.MemberName, d.Email, d.Title, d.DaysLate}
	default:
		return nil
	}
	return chargeLateLoan(ctx, tx, l)
}

// chargeLateLoan creates the fine for a loan, or raises it if the book is now later.
// It never charges the same loan twice.
func chargeLateLoan(ctx context.Context, tx *sql.Tx, l lateLoan) error {
	amount := float64(l.daysLate) * finePerDay
	var existing struct {
		id     int64
		amount float64
		status string
	}
	err := tx.QueryRowContext(ctx, `SELECT id, amount::float8, status FROM fines WHERE loan_id=$1 FOR UPDATE`, l.loanID).
		Scan(&existing.id, &existing.amount, &existing.status)
	var fineID int64
	switch {
	case err == sql.ErrNoRows:
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO fines (loan_id, member_id, member_name, email, title, days_late, amount)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			l.loanID, l.memberID, l.name, l.email, l.title, l.daysLate, amount).Scan(&fineID); err != nil {
			return err
		}
	case err != nil:
		return err
	case existing.status == "unpaid" && amount > existing.amount:
		fineID = existing.id
		if _, err := tx.ExecContext(ctx, `UPDATE fines SET amount=$2, days_late=$3 WHERE id=$1`, fineID, amount, l.daysLate); err != nil {
			return err
		}
	default:
		log.Printf("[fine] loan %d already fined Nu.%.2f — nothing to change", l.loanID, existing.amount)
		return nil
	}

	total, err := outstanding(ctx, tx, l.memberID)
	if err != nil {
		return err
	}
	log.Printf("[fine] %s fined Nu.%.2f for %q (%d day(s) late); outstanding Nu.%.2f", l.name, amount, l.title, l.daysLate, total)
	// EVENT CHAIN: the fine is announced so Member can suspend and Notification can alert.
	_, err = store.Enqueue(ctx, tx, service, events.FineIssued, events.FineIssuedData{
		FineID: fineID, LoanID: l.loanID, MemberID: l.memberID, MemberName: l.name, Email: l.email,
		Title: l.title, Amount: amount, DaysLate: l.daysLate, TotalOutstanding: total})
	return err
}

func outstanding(ctx context.Context, tx *sql.Tx, memberID int64) (float64, error) {
	var total float64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount),0)::float8 FROM fines WHERE member_id=$1 AND status='unpaid'`, memberID).Scan(&total)
	return total, err
}

// ---------- HTTP ----------

func listFines(w http.ResponseWriter, r *http.Request) {
	q := `SELECT id, loan_id, member_id, member_name, title, days_late, amount::float8, status, created_at, paid_at FROM fines`
	var args []any
	if v := r.URL.Query().Get("memberId"); v != "" {
		q += ` WHERE member_id=$1`
		args = append(args, v)
	}
	rows, err := db.QueryContext(r.Context(), q+` ORDER BY id DESC`, args...)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	list := []Fine{}
	for rows.Next() {
		var f Fine
		var paid sql.NullTime
		if err := rows.Scan(&f.ID, &f.LoanID, &f.MemberID, &f.MemberName, &f.Title, &f.DaysLate, &f.Amount, &f.Status, &f.CreatedAt, &paid); err != nil {
			svc.Error(w, 500, "%v", err)
			return
		}
		if paid.Valid {
			f.PaidAt = &paid.Time
		}
		list = append(list, f)
	}
	svc.JSON(w, 200, list)
}

func payFine(w http.ResponseWriter, r *http.Request) {
	id, err := svc.PathID(r, "id")
	if err != nil {
		svc.Error(w, 400, "invalid fine id")
		return
	}
	ctx := r.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck
	var d events.FinePaidData
	var status string
	err = tx.QueryRowContext(ctx, `SELECT id, member_id, member_name, email, amount::float8, status FROM fines WHERE id=$1 FOR UPDATE`, id).
		Scan(&d.FineID, &d.MemberID, &d.MemberName, &d.Email, &d.Amount, &status)
	if err == sql.ErrNoRows {
		svc.Error(w, 404, "fine %d not found", id)
		return
	} else if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if status == "paid" {
		svc.Error(w, 409, "fine %d is already paid", id)
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fines SET status='paid', paid_at=now() WHERE id=$1`, id); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if d.TotalOutstanding, err = outstanding(ctx, tx, d.MemberID); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if _, err := store.Enqueue(ctx, tx, service, events.FinePaid, d); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	svc.JSON(w, 200, d)
}

func getFailureMode(w http.ResponseWriter, r *http.Request) {
	svc.JSON(w, 200, map[string]bool{"enabled": failureMode.Load()})
}

func setFailureMode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := svc.Decode(r, &in); err != nil {
		svc.Error(w, 400, "body must be {\"enabled\": true|false}")
		return
	}
	failureMode.Store(in.Enabled)
	log.Printf("[fine] failure mode set to %v", in.Enabled)
	svc.JSON(w, 200, map[string]bool{"enabled": in.Enabled})
}
