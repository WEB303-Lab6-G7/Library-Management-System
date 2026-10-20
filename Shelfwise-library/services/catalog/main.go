// Catalog Service — owns books and their physical copies.
// Publishes: BookAdded
// Consumes:  BookBorrowed, BookReturned, HoldReady (to keep copy status in sync)
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"

	"library/pkg/events"
	"library/pkg/store"
	"library/pkg/svc"
)

const service = "catalog"

var db *sql.DB

var schema = []string{
	`CREATE TABLE IF NOT EXISTS books (
		id         BIGSERIAL PRIMARY KEY,
		title      TEXT NOT NULL,
		author     TEXT NOT NULL,
		isbn       TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS copies (
		id              BIGSERIAL PRIMARY KEY,
		book_id         BIGINT NOT NULL REFERENCES books(id),
		status          TEXT NOT NULL DEFAULT 'available', -- available | on_loan | on_hold
		held_for_member BIGINT
	)`,
	`ALTER TABLE books ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT 'General'`,
	`ALTER TABLE books ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT ''`,
}

type Book struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	ISBN        string `json:"isbn"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Total       int    `json:"totalCopies"`
	Available   int    `json:"availableCopies"`
	OnLoan      int    `json:"onLoan"`
	OnHold      int    `json:"onHold"`
}

func main() {
	var err error
	db, err = store.Open(svc.Env("DATABASE_URL", "postgres://library:library@localhost:5432/catalog_db?sslmode=disable"), schema...)
	if err != nil {
		log.Fatal(err)
	}
	broker, err := events.Connect(svc.Env("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/"), service)
	if err != nil {
		log.Fatal(err)
	}
	go store.RunOutboxRelay(db, broker)
	if svc.Env("SEED_BOOKS", "true") == "true" {
		seedBooks()
	}

	if err := broker.Subscribe("catalog.events",
		[]string{events.BookBorrowed, events.BookReturned, events.HoldReady},
		store.Idempotent(db, handleEvent)); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /books", listBooks)
	mux.HandleFunc("POST /books", addBook)
	mux.HandleFunc("GET /books/{id}", getBook)
	mux.HandleFunc("GET /books/{id}/available-copy", availableCopy)
	svc.Serve(service, ":"+svc.Env("PORT", "8081"), mux)
}

// ---------- Event handlers ----------

func handleEvent(ctx context.Context, tx *sql.Tx, env events.Envelope) error {
	switch env.EventType {
	case events.BookBorrowed:
		var d events.BookBorrowedData
		if err := env.Decode(&d); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE copies SET status='on_loan', held_for_member=NULL WHERE id=$1`, d.CopyID)
		log.Printf("[catalog] copy %d of %q is now on loan", d.CopyID, d.Title)
		return err

	case events.BookReturned:
		var d events.BookReturnedData
		if err := env.Decode(&d); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE copies SET status='available', held_for_member=NULL WHERE id=$1`, d.CopyID)
		log.Printf("[catalog] copy %d of %q is available again", d.CopyID, d.Title)
		return err

	case events.HoldReady:
		var d events.HoldReadyData
		if err := env.Decode(&d); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE copies SET status='on_hold', held_for_member=$2 WHERE id=$1 AND status='available'`, d.CopyID, d.MemberID)
		log.Printf("[catalog] copy %d of %q set aside for member %d", d.CopyID, d.Title, d.MemberID)
		return err
	}
	return nil
}

// ---------- HTTP ----------

const bookQuery = `
	SELECT b.id, b.title, b.author, b.isbn, b.category, b.description,
	       COUNT(c.id),
	       COUNT(c.id) FILTER (WHERE c.status='available'),
	       COUNT(c.id) FILTER (WHERE c.status='on_loan'),
	       COUNT(c.id) FILTER (WHERE c.status='on_hold')
	FROM books b LEFT JOIN copies c ON c.book_id = b.id`

func scanBook(s interface{ Scan(...any) error }) (Book, error) {
	var b Book
	err := s.Scan(&b.ID, &b.Title, &b.Author, &b.ISBN, &b.Category, &b.Description, &b.Total, &b.Available, &b.OnLoan, &b.OnHold)
	return b, err
}

func listBooks(w http.ResponseWriter, r *http.Request) {
	rows, err := db.QueryContext(r.Context(), bookQuery+` GROUP BY b.id ORDER BY b.id`)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	books := []Book{}
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			svc.Error(w, 500, "%v", err)
			return
		}
		books = append(books, b)
	}
	svc.JSON(w, 200, books)
}

func getBook(w http.ResponseWriter, r *http.Request) {
	id, err := svc.PathID(r, "id")
	if err != nil {
		svc.Error(w, 400, "invalid book id")
		return
	}
	b, err := scanBook(db.QueryRowContext(r.Context(), bookQuery+` WHERE b.id=$1 GROUP BY b.id`, id))
	if err == sql.ErrNoRows {
		svc.Error(w, 404, "book %d not found", id)
		return
	} else if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	svc.JSON(w, 200, b)
}

type newBook struct {
	Title       string `json:"title"`
	Author      string `json:"author"`
	ISBN        string `json:"isbn"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Copies      int    `json:"copies"`
}

func addBook(w http.ResponseWriter, r *http.Request) {
	var in newBook
	if err := svc.Decode(r, &in); err != nil || strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Author) == "" {
		svc.Error(w, 400, "title and author are required")
		return
	}
	b, err := createBook(r.Context(), in)
	if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	svc.JSON(w, 201, b)
}

// createBook stores the book and its copies and queues BookAdded, all in one transaction.
func createBook(ctx context.Context, in newBook) (Book, error) {
	in.Title, in.Author = strings.TrimSpace(in.Title), strings.TrimSpace(in.Author)
	if in.Copies < 1 {
		in.Copies = 1
	}
	if strings.TrimSpace(in.Category) == "" {
		in.Category = "General"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Book{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO books (title, author, isbn, category, description) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		in.Title, in.Author, in.ISBN, in.Category, in.Description).Scan(&id); err != nil {
		return Book{}, err
	}
	for i := 0; i < in.Copies; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO copies (book_id) VALUES ($1)`, id); err != nil {
			return Book{}, err
		}
	}
	if _, err := store.Enqueue(ctx, tx, service, events.BookAdded,
		events.BookAddedData{BookID: id, Title: in.Title, Author: in.Author, Copies: in.Copies}); err != nil {
		return Book{}, err
	}
	if err := tx.Commit(); err != nil {
		return Book{}, err
	}
	return Book{ID: id, Title: in.Title, Author: in.Author, ISBN: in.ISBN, Category: in.Category,
		Description: in.Description, Total: in.Copies, Available: in.Copies}, nil
}

// seedBooks fills an empty catalogue with a starter collection so the site is not blank.
func seedBooks() {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&n); err != nil || n > 0 {
		return
	}
	starter := []newBook{
		{"The Circle of Karma", "Kunzang Choden", "", "Bhutanese Literature", "A young woman's journey across Bhutan and India in search of her own path.", 2},
		{"Folktales of Bhutan", "Kunzang Choden", "", "Bhutanese Literature", "Stories of ghosts, kings and clever villagers passed down through generations.", 3},
		{"Treasures of the Thunder Dragon", "Ashi Dorji Wangmo Wangchuck", "", "History", "A portrait of Bhutan, its people and its traditions.", 1},
		{"The Raven Crown", "Michael Aris", "", "History", "The origins of Buddhist monarchy in Bhutan.", 1},
		{"Beyond the Sky and the Earth", "Jamie Zeppa", "", "Memoir", "A Canadian teacher's years in eastern Bhutan.", 2},
		{"Building Microservices", "Sam Newman", "", "Software Engineering", "Designing fine-grained systems that can be deployed independently.", 2},
		{"Designing Data-Intensive Applications", "Martin Kleppmann", "", "Software Engineering", "The ideas behind reliable, scalable and maintainable systems.", 1},
		{"Enterprise Integration Patterns", "Gregor Hohpe & Bobby Woolf", "", "Software Engineering", "Messaging patterns for connecting applications.", 1},
		{"The Pragmatic Programmer", "David Thomas & Andrew Hunt", "", "Software Engineering", "Practical advice for the working developer.", 2},
		{"Clean Code", "Robert C. Martin", "", "Software Engineering", "A handbook of agile software craftsmanship.", 3},
	}
	for _, b := range starter {
		if _, err := createBook(context.Background(), b); err != nil {
			log.Printf("[catalog] seed failed: %v", err)
			return
		}
	}
	log.Printf("[catalog] seeded %d starter books", len(starter))
}

// availableCopy is the one synchronous query Circulation makes before lending:
// "give me a copy this member may take". A copy on hold counts only for the member it is held for.
func availableCopy(w http.ResponseWriter, r *http.Request) {
	id, err := svc.PathID(r, "id")
	if err != nil {
		svc.Error(w, 400, "invalid book id")
		return
	}
	memberID := r.URL.Query().Get("memberId")
	// Copies Circulation has lent out but whose BookBorrowed we may not have processed yet.
	exclude := "{" + r.URL.Query().Get("exclude") + "}"
	var title string
	if err := db.QueryRowContext(r.Context(), `SELECT title FROM books WHERE id=$1`, id).Scan(&title); err == sql.ErrNoRows {
		svc.Error(w, 404, "book %d not found", id)
		return
	} else if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	var copyID int64
	err = db.QueryRowContext(r.Context(), `
		SELECT id FROM copies
		WHERE book_id=$1 AND (status='available' OR (status='on_hold' AND held_for_member::text=$2))
		  AND NOT (id = ANY($3::bigint[]))
		ORDER BY (status='on_hold') DESC, id LIMIT 1`, id, memberID, exclude).Scan(&copyID)
	if err == sql.ErrNoRows {
		svc.Error(w, 409, "no copies of %q are available — place a hold instead", title)
		return
	} else if err != nil {
		svc.Error(w, 500, "%v", err)
		return
	}
	svc.JSON(w, 200, map[string]any{"bookId": id, "copyId": copyID, "title": title})
}
