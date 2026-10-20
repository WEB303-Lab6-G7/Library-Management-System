// Circulation Service — owns loans (borrow / return) and detects overdue books.
// Publishes: BookBorrowed, BookReturned, BookOverdue
// Consumes:  MemberRegistered, MemberSuspended, MemberReactivated
//
// Circulation never reads the Member database. It keeps its own local copy of
// each member's status (member_view), kept up to date by Member events.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"library/pkg/events"
	"library/pkg/store"
	"library/pkg/svc"
)

const service = "circulation"

var (
	db         *sql.DB
	catalogURL string
	loanDays   int
)

var schema = []string{
	`CREATE TABLE IF NOT EXISTS member_view (
		member_id    BIGINT PRIMARY KEY,
		name         TEXT NOT NULL,
		email        TEXT NOT NULL,
		status       TEXT NOT NULL,
		borrow_limit INT  NOT NULL,
		updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS loans (
		id               BIGSERIAL PRIMARY KEY,
		member_id        BIGINT NOT NULL,
		member_name      TEXT NOT NULL,
		email            TEXT NOT NULL,
		book_id          BIGINT NOT NULL,
		copy_id          BIGINT NOT NULL,
		title            TEXT NOT NULL,
		borrowed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
		due_date         TIMESTAMPTZ NOT NULL,
		returned_at      TIMESTAMPTZ,
		days_late        INT NOT NULL DEFAULT 0,
		overdue_notified BOOLEAN NOT NULL DEFAULT false
	)`,
	// A physical copy can only be on one open loan at a time.
	`CREATE UNIQUE INDEX IF NOT EXISTS one_open_loan_per_copy ON loans (copy_id) WHERE returned_at IS NULL`,
}

type Loan struct {
	ID         int64      `json:"id"`
	MemberID   int64      `json:"memberId"`
	MemberName string     `json:"memberName"`
	BookID     int64      `json:"bookId"`
	CopyID     int64      `json:"copyId"`
	Title      string     `json:"title"`
	BorrowedAt time.Time  `json:"borrowedAt"`
	DueDate    time.Time  `json:"dueDate"`
	ReturnedAt *time.Time `json:"returnedAt"`
	DaysLate   int        `json:"daysLate"`
	Overdue    bool       `json:"overdue"`
}

type memberView struct {
	ID          int64  `json:"memberId"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	Status      string `json:"status"`
	BorrowLimit int    `json:"borrowLimit"`
}

func main() {
	catalogURL = svc.Env("CATALOG_URL", "http://localhost:8081")
	loanDays = svc.EnvInt("LOAN_DAYS", 14)
	var err error
	db, err = store.Open(svc.Env("DATABASE_URL", "postgres://library:library@localhost:5432/circulation_db?sslmode=disable"), schema...)
	if err != nil {
		log.Fatal(err)
	}
	broker, err := events.Connect(svc.Env("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/"), service)
	if err != nil {
		log.Fatal(err)
	}
	go store.RunOutboxRelay(db, broker)
	go overdueChecker(time.Duration(svc.EnvInt("OVERDUE_CHECK_SECONDS", 20)) * time.Second)

	if err := broker.Subscribe("circulation.events",
		[]string{events.MemberRegistered, events.MemberSuspended, events.MemberReactivated},
		store.Idempotent(db, handleEvent)); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /loans", listLoans)
	mux.HandleFunc("POST /loans", borrow)
	mux.HandleFunc("POST /loans/{id}/return", returnBook)
	mux.HandleFunc("GET /member-view", listMemberView)
	svc.Serve(service, ":"+svc.Env("PORT", "8083"), mux)
}

// ---------- Event handlers: keep the local member view in sync ----------

func handleEvent(ctx context.Context, tx *sql.Tx, env events.Envelope) error {
	switch env.EventType {
	case events.MemberRegistered:
		var d events.MemberRegisteredData
		if err := env.Decode(&d); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO member_view (member_id, name, email, status, borrow_limit) VALUES ($1,$2,$3,'active',$4)
			ON CONFLICT (member_id) DO UPDATE SET name=$2, email=$3, borrow_limit=$4, updated_at=now()`,
			d.MemberID, d.Name, d.Email, d.BorrowLimit)
		return err
	case events.MemberSuspended, events.MemberReactivated:
		var d events.MemberStatusData
		if err := env.Decode(&d); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE member_view SET status=$2, updated_at=now() WHERE member_id=$1`, d.MemberID, d.Status)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("member %d not in local view yet", d.MemberID) // retry later
		}
		log.Printf("[circulation] member %d is now %s — borrowing %s", d.MemberID, d.Status,
			map[string]string{"active": "allowed", "suspended": "blocked"}[d.Status])
	}
	return nil
}

// ---------- Overdue detection (publishes BookOverdue) ----------

func overdueChecker(every time.Duration) {
	for {
		time.Sleep(every)
		if err := checkOverdue(); err != nil {
			log.Printf("[overdue] %v", err)
		}
	}
}

func checkOverdue() error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	rows, err := tx.QueryContext(ctx, `
		SELECT id, member_id, member_name, email, book_id, title, due_date FROM loans
		WHERE returned_at IS NULL AND due_date < now() AND NOT overdue_notified
		FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	var due []events.BookOverdueData
	for rows.Next() {
		var d events.BookOverdueData
		if err := rows.Scan(&d.LoanID, &d.MemberID, &d.MemberName, &d.Email, &d.BookID, &d.Title, &d.DueDate); err != nil {
			rows.Close()
			return err
		}
		d.DaysLate = daysLate(d.DueDate, time.Now())
		due = append(due, d)
	}
	rows.Close()
	for _, d := range due {
		if _, err := tx.ExecContext(ctx, `UPDATE loans SET overdue_notified=true WHERE id=$1`, d.LoanID); err != nil {
			return err
		}
		if _, err := store.Enqueue(ctx, tx, service, events.BookOverdue, d); err != nil {
			return err
		}
		log.Printf("[overdue] loan %d (%q) is %d day(s) overdue", d.LoanID, d.Title, d.DaysLate)
	}
	return tx.Commit()
}

func daysLate(due, at time.Time) int {
	if !at.After(due) {
		return 0
	}
	// Whole days only: returning a few hours after the due time is not yet a day late.
	return int(math.Floor(at.Sub(due).Hours() / 24))
}

// ---------- HTTP ----------

func borrow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MemberID int64 `json:"memberId"`
		BookID   int64 `json:"bookId"`
		// DueInDays overrides the loan period. Negative values back-date the loan,
		// which is how the demo creates overdue books without waiting two weeks.
		DueInDays *int `json:"dueInDays"`
	}
	if err := svc.Decode(r, &in); err != nil || in.MemberID == 0 || in.BookID == 0 {
		svc.Error(w, 400, "memberId and bookId are required")
		return
	}
	ctx := r.Context()

	// 1. Check the member using our OWN local view (no call to Member service).
	var m memberView
	err := db.QueryRowContext(ctx, `SELECT member_id, name, email, status, borrow_limit FROM member_view WHERE member_id=$1`,
		in.MemberID).Scan(&m.ID, &m.Name, &m.Email, &m.Status, &m.BorrowLimit)
	if err == sql.ErrNoRows {
		svc.Error(w, 404, "member %d is unknown to circulation (not registered, or registration event not processed yet)", in.MemberID)
		return
	} else if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if m.Status != "active" {
		svc.Error(w, 403, "%s is suspended and cannot borrow until fines are paid", m.Name)
		return
	}
	var open int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM loans WHERE member_id=$1 AND returned_at IS NULL`, m.ID).Scan(&open); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if open >= m.BorrowLimit {
		svc.Error(w, 409, "%s already has %d book(s) — the limit is %d", m.Name, open, m.BorrowLimit)
		return
	}

	// 2. Ask Catalog (synchronously) for a copy this member may take. We pass the
	//    copies we have already lent out, in case Catalog has not processed those
	//    BookBorrowed events yet (eventual consistency).
	var lent []string
	if rows, err := db.QueryContext(ctx, `SELECT copy_id FROM loans WHERE book_id=$1 AND returned_at IS NULL`, in.BookID); err == nil {
		for rows.Next() {
			var c int64
			if rows.Scan(&c) == nil {
				lent = append(lent, fmt.Sprint(c))
			}
		}
		rows.Close()
	}
	var copy struct {
		CopyID int64  `json:"copyId"`
		Title  string `json:"title"`
	}
	if status, err := svc.GetJSON(fmt.Sprintf("%s/books/%d/available-copy?memberId=%d&exclude=%s", catalogURL, in.BookID, m.ID, strings.Join(lent, ",")), &copy); err != nil {
		if status == 0 {
			svc.Error(w, 503, "catalog service is unavailable: %v", err)
		} else {
			svc.Error(w, status, "%v", err)
		}
		return
	}

	// 3. Record the loan and the BookBorrowed event in one transaction.
	days := loanDays
	if in.DueInDays != nil {
		days = *in.DueInDays
	}
	now := time.Now().UTC()
	l := Loan{MemberID: m.ID, MemberName: m.Name, BookID: in.BookID, CopyID: copy.CopyID, Title: copy.Title,
		BorrowedAt: now, DueDate: now.AddDate(0, 0, days)}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck
	err = tx.QueryRowContext(ctx, `
		INSERT INTO loans (member_id, member_name, email, book_id, copy_id, title, borrowed_at, due_date)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		m.ID, m.Name, m.Email, l.BookID, l.CopyID, l.Title, l.BorrowedAt, l.DueDate).Scan(&l.ID)
	if err != nil {
		if strings.Contains(err.Error(), "one_open_loan_per_copy") {
			svc.Error(w, 409, "that copy was just lent to someone else, please try again")
			return
		}
		svc.Error(w, 500, "%v", err)
		return
	}
	if _, err := store.Enqueue(ctx, tx, service, events.BookBorrowed, events.BookBorrowedData{
		LoanID: l.ID, MemberID: m.ID, MemberName: m.Name, Email: m.Email, BookID: l.BookID, CopyID: l.CopyID,
		Title: l.Title, BorrowedAt: l.BorrowedAt, DueDate: l.DueDate}); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	svc.JSON(w, 201, l)
}

func returnBook(w http.ResponseWriter, r *http.Request) {
	id, err := svc.PathID(r, "id")
	if err != nil {
		svc.Error(w, 400, "invalid loan id")
		return
	}
	ctx := r.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	var d events.BookReturnedData
	var returned sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT id, member_id, member_name, email, book_id, copy_id, title, due_date, returned_at
		FROM loans WHERE id=$1 FOR UPDATE`, id).
		Scan(&d.LoanID, &d.MemberID, &d.MemberName, &d.Email, &d.BookID, &d.CopyID, &d.Title, &d.DueDate, &returned)
	if err == sql.ErrNoRows {
		svc.Error(w, 404, "loan %d not found", id)
		return
	} else if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if returned.Valid {
		svc.Error(w, 409, "loan %d was already returned", id)
		return
	}
	d.ReturnedAt = time.Now().UTC()
	d.DaysLate = daysLate(d.DueDate, d.ReturnedAt)
	if _, err := tx.ExecContext(ctx, `UPDATE loans SET returned_at=$2, days_late=$3 WHERE id=$1`, id, d.ReturnedAt, d.DaysLate); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if _, err := store.Enqueue(ctx, tx, service, events.BookReturned, d); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	svc.JSON(w, 200, map[string]any{"loanId": id, "title": d.Title, "returnedAt": d.ReturnedAt, "daysLate": d.DaysLate})
}

func listLoans(w http.ResponseWriter, r *http.Request) {
	q := `SELECT id, member_id, member_name, book_id, copy_id, title, borrowed_at, due_date, returned_at, days_late FROM loans`
	var args []any
	var where []string
	if v := r.URL.Query().Get("memberId"); v != "" {
		args = append(args, v)
		where = append(where, fmt.Sprintf("member_id=$%d", len(args)))
	}
	if r.URL.Query().Get("active") == "true" {
		where = append(where, "returned_at IS NULL")
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := db.QueryContext(r.Context(), q+" ORDER BY id DESC", args...)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	list := []Loan{}
	now := time.Now()
	for rows.Next() {
		var l Loan
		var ret sql.NullTime
		if err := rows.Scan(&l.ID, &l.MemberID, &l.MemberName, &l.BookID, &l.CopyID, &l.Title, &l.BorrowedAt, &l.DueDate, &ret, &l.DaysLate); err != nil {
			svc.Error(w, 500, "%v", err)
			return
		}
		if ret.Valid {
			l.ReturnedAt = &ret.Time
		} else {
			l.Overdue = now.After(l.DueDate)
			l.DaysLate = daysLate(l.DueDate, now)
		}
		list = append(list, l)
	}
	svc.JSON(w, 200, list)
}

func listMemberView(w http.ResponseWriter, r *http.Request) {
	rows, err := db.QueryContext(r.Context(), `SELECT member_id, name, email, status, borrow_limit FROM member_view ORDER BY member_id`)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	list := []memberView{}
	for rows.Next() {
		var m memberView
		if err := rows.Scan(&m.ID, &m.Name, &m.Email, &m.Status, &m.BorrowLimit); err != nil {
			svc.Error(w, 500, "%v", err)
			return
		}
		list = append(list, m)
	}
	svc.JSON(w, 200, list)
}
