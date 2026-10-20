// Reservation Service — owns the hold (waitlist) queue for books with no free copy.
// Publishes: HoldPlaced, HoldReady
// Consumes:  MemberRegistered (local member view), BookReturned (serve the queue),
//
//	BookBorrowed (mark a ready hold as collected)
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"time"

	"library/pkg/events"
	"library/pkg/store"
	"library/pkg/svc"
)

const service = "reservation"

var (
	db         *sql.DB
	catalogURL string
)

var schema = []string{
	`CREATE TABLE IF NOT EXISTS member_view (
		member_id BIGINT PRIMARY KEY,
		name      TEXT NOT NULL,
		email     TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS holds (
		id          BIGSERIAL PRIMARY KEY,
		member_id   BIGINT NOT NULL,
		member_name TEXT NOT NULL,
		email       TEXT NOT NULL,
		book_id     BIGINT NOT NULL,
		title       TEXT NOT NULL,
		status      TEXT NOT NULL DEFAULT 'waiting', -- waiting | ready | collected
		copy_id     BIGINT,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
		ready_at    TIMESTAMPTZ,
		expires_at  TIMESTAMPTZ
	)`,
}

type Hold struct {
	ID         int64      `json:"id"`
	MemberID   int64      `json:"memberId"`
	MemberName string     `json:"memberName"`
	BookID     int64      `json:"bookId"`
	Title      string     `json:"title"`
	Status     string     `json:"status"`
	CopyID     *int64     `json:"copyId"`
	Position   int        `json:"position"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt"`
}

func main() {
	catalogURL = svc.Env("CATALOG_URL", "http://localhost:8081")
	var err error
	db, err = store.Open(svc.Env("DATABASE_URL", "postgres://library:library@localhost:5432/reservation_db?sslmode=disable"), schema...)
	if err != nil {
		log.Fatal(err)
	}
	broker, err := events.Connect(svc.Env("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/"), service)
	if err != nil {
		log.Fatal(err)
	}
	go store.RunOutboxRelay(db, broker)

	if err := broker.Subscribe("reservation.events",
		[]string{events.MemberRegistered, events.BookReturned, events.BookBorrowed},
		store.Idempotent(db, handleEvent)); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /holds", listHolds)
	mux.HandleFunc("POST /holds", placeHold)
	svc.Serve(service, ":"+svc.Env("PORT", "8084"), mux)
}

// ---------- Event handlers ----------

func handleEvent(ctx context.Context, tx *sql.Tx, env events.Envelope) error {
	switch env.EventType {
	case events.MemberRegistered:
		var d events.MemberRegisteredData
		if err := env.Decode(&d); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO member_view (member_id, name, email) VALUES ($1,$2,$3)
			ON CONFLICT (member_id) DO UPDATE SET name=$2, email=$3`, d.MemberID, d.Name, d.Email)
		return err

	case events.BookReturned:
		// EVENT CHAIN: a returned book may satisfy the first person in the queue.
		var d events.BookReturnedData
		if err := env.Decode(&d); err != nil {
			return err
		}
		var h events.HoldReadyData
		err := tx.QueryRowContext(ctx, `
			SELECT id, member_id, member_name, email, book_id, title FROM holds
			WHERE book_id=$1 AND status='waiting' ORDER BY created_at, id LIMIT 1 FOR UPDATE`, d.BookID).
			Scan(&h.HoldID, &h.MemberID, &h.MemberName, &h.Email, &h.BookID, &h.Title)
		if err == sql.ErrNoRows {
			log.Printf("[reservation] %q returned — nobody waiting", d.Title)
			return nil
		} else if err != nil {
			return err
		}
		h.CopyID = d.CopyID
		h.ExpiresAt = time.Now().UTC().Add(72 * time.Hour)
		if _, err := tx.ExecContext(ctx, `UPDATE holds SET status='ready', copy_id=$2, ready_at=now(), expires_at=$3 WHERE id=$1`,
			h.HoldID, h.CopyID, h.ExpiresAt); err != nil {
			return err
		}
		log.Printf("[reservation] hold %d: %q is ready for %s", h.HoldID, h.Title, h.MemberName)
		_, err = store.Enqueue(ctx, tx, service, events.HoldReady, h)
		return err

	case events.BookBorrowed:
		var d events.BookBorrowedData
		if err := env.Decode(&d); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE holds SET status='collected' WHERE member_id=$1 AND book_id=$2 AND status IN ('ready','waiting')`,
			d.MemberID, d.BookID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("[reservation] %s collected held book %q", d.MemberName, d.Title)
		}
	}
	return nil
}

// ---------- HTTP ----------

func placeHold(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MemberID int64 `json:"memberId"`
		BookID   int64 `json:"bookId"`
	}
	if err := svc.Decode(r, &in); err != nil || in.MemberID == 0 || in.BookID == 0 {
		svc.Error(w, 400, "memberId and bookId are required")
		return
	}
	ctx := r.Context()
	var name, email string
	if err := db.QueryRowContext(ctx, `SELECT name, email FROM member_view WHERE member_id=$1`, in.MemberID).Scan(&name, &email); err == sql.ErrNoRows {
		svc.Error(w, 404, "member %d is unknown to the reservation service", in.MemberID)
		return
	} else if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}

	// Synchronous check with Catalog: holds only make sense when nothing is on the shelf.
	var book struct {
		Title     string `json:"title"`
		Available int    `json:"availableCopies"`
	}
	if status, err := svc.GetJSON(fmt.Sprintf("%s/books/%d", catalogURL, in.BookID), &book); err != nil {
		if status == 0 {
			svc.Error(w, 503, "catalog service is unavailable: %v", err)
		} else {
			svc.Error(w, status, "%v", err)
		}
		return
	}
	if book.Available > 0 {
		svc.Error(w, 409, "%q has %d copy/copies on the shelf — borrow it directly", book.Title, book.Available)
		return
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck
	var exists bool
	_ = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM holds WHERE member_id=$1 AND book_id=$2 AND status IN ('waiting','ready'))`,
		in.MemberID, in.BookID).Scan(&exists)
	if exists {
		svc.Error(w, 409, "%s already has a hold on %q", name, book.Title)
		return
	}
	var d events.HoldPlacedData
	if err := tx.QueryRowContext(ctx, `INSERT INTO holds (member_id, member_name, email, book_id, title) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		in.MemberID, name, email, in.BookID, book.Title).Scan(&d.HoldID); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM holds WHERE book_id=$1 AND status='waiting'`, in.BookID).Scan(&d.Position)
	d.MemberID, d.MemberName, d.Email, d.BookID, d.Title = in.MemberID, name, email, in.BookID, book.Title
	if _, err := store.Enqueue(ctx, tx, service, events.HoldPlaced, d); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	svc.JSON(w, 201, d)
}

func listHolds(w http.ResponseWriter, r *http.Request) {
	q := `SELECT h.id, h.member_id, h.member_name, h.book_id, h.title, h.status, h.copy_id, h.created_at, h.expires_at,
	        CASE WHEN h.status='waiting' THEN
	          (SELECT COUNT(*) FROM holds x WHERE x.book_id=h.book_id AND x.status='waiting' AND (x.created_at, x.id) <= (h.created_at, h.id))
	        ELSE 0 END
	      FROM holds h`
	var args []any
	if v := r.URL.Query().Get("memberId"); v != "" {
		q += ` WHERE h.member_id=$1`
		args = append(args, v)
	}
	rows, err := db.QueryContext(r.Context(), q+` ORDER BY h.id DESC`, args...)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	list := []Hold{}
	for rows.Next() {
		var h Hold
		var copyID sql.NullInt64
		var exp sql.NullTime
		if err := rows.Scan(&h.ID, &h.MemberID, &h.MemberName, &h.BookID, &h.Title, &h.Status, &copyID, &h.CreatedAt, &exp, &h.Position); err != nil {
			svc.Error(w, 500, "%v", err)
			return
		}
		if copyID.Valid {
			h.CopyID = &copyID.Int64
		}
		if exp.Valid {
			h.ExpiresAt = &exp.Time
		}
		list = append(list, h)
	}
	svc.JSON(w, 200, list)
}
