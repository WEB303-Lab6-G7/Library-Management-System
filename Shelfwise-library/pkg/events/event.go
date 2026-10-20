// Package events defines the event contract shared by every service.
// It is the single place where event names and payloads are agreed on.
package events

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Exchange is the durable topic exchange all services publish to.
const Exchange = "library.events"

// Event type names. They are also used as RabbitMQ routing keys.
const (
	MemberRegistered  = "MemberRegistered"
	MemberSuspended   = "MemberSuspended"
	MemberReactivated = "MemberReactivated"
	BookAdded         = "BookAdded"
	BookBorrowed      = "BookBorrowed"
	BookReturned      = "BookReturned"
	BookOverdue       = "BookOverdue"
	FineIssued        = "FineIssued"
	FinePaid          = "FinePaid"
	HoldPlaced        = "HoldPlaced"
	HoldReady         = "HoldReady"
)

// Envelope is what actually travels on the wire.
type Envelope struct {
	EventID    string          `json:"eventId"`
	EventType  string          `json:"eventType"`
	Source     string          `json:"source"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

// New wraps a payload in an envelope with a fresh unique ID.
func New(eventType, source string, payload any) (Envelope, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		EventID:    uuid.NewString(),
		EventType:  eventType,
		Source:     source,
		OccurredAt: time.Now().UTC(),
		Data:       b,
	}, nil
}

// Decode unmarshals the envelope payload into v.
func (e Envelope) Decode(v any) error { return json.Unmarshal(e.Data, v) }

// ---- Payloads ----
// Each payload carries enough data that consumers never need to call the publisher back.

type MemberRegisteredData struct {
	MemberID    int64  `json:"memberId"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	BorrowLimit int    `json:"borrowLimit"`
}

type MemberStatusData struct {
	MemberID         int64   `json:"memberId"`
	Name             string  `json:"name"`
	Email            string  `json:"email"`
	Status           string  `json:"status"` // active | suspended
	Reason           string  `json:"reason"`
	OutstandingFines float64 `json:"outstandingFines"`
}

type BookAddedData struct {
	BookID int64  `json:"bookId"`
	Title  string `json:"title"`
	Author string `json:"author"`
	Copies int    `json:"copies"`
}

type BookBorrowedData struct {
	LoanID     int64     `json:"loanId"`
	MemberID   int64     `json:"memberId"`
	MemberName string    `json:"memberName"`
	Email      string    `json:"email"`
	BookID     int64     `json:"bookId"`
	CopyID     int64     `json:"copyId"`
	Title      string    `json:"title"`
	BorrowedAt time.Time `json:"borrowedAt"`
	DueDate    time.Time `json:"dueDate"`
}

type BookReturnedData struct {
	LoanID     int64     `json:"loanId"`
	MemberID   int64     `json:"memberId"`
	MemberName string    `json:"memberName"`
	Email      string    `json:"email"`
	BookID     int64     `json:"bookId"`
	CopyID     int64     `json:"copyId"`
	Title      string    `json:"title"`
	DueDate    time.Time `json:"dueDate"`
	ReturnedAt time.Time `json:"returnedAt"`
	DaysLate   int       `json:"daysLate"`
}

type BookOverdueData struct {
	LoanID     int64     `json:"loanId"`
	MemberID   int64     `json:"memberId"`
	MemberName string    `json:"memberName"`
	Email      string    `json:"email"`
	BookID     int64     `json:"bookId"`
	Title      string    `json:"title"`
	DueDate    time.Time `json:"dueDate"`
	DaysLate   int       `json:"daysLate"`
}

type FineIssuedData struct {
	FineID           int64   `json:"fineId"`
	LoanID           int64   `json:"loanId"`
	MemberID         int64   `json:"memberId"`
	MemberName       string  `json:"memberName"`
	Email            string  `json:"email"`
	Title            string  `json:"title"`
	Amount           float64 `json:"amount"`
	DaysLate         int     `json:"daysLate"`
	TotalOutstanding float64 `json:"totalOutstanding"`
}

type FinePaidData struct {
	FineID           int64   `json:"fineId"`
	MemberID         int64   `json:"memberId"`
	MemberName       string  `json:"memberName"`
	Email            string  `json:"email"`
	Amount           float64 `json:"amount"`
	TotalOutstanding float64 `json:"totalOutstanding"`
}

type HoldPlacedData struct {
	HoldID     int64  `json:"holdId"`
	MemberID   int64  `json:"memberId"`
	MemberName string `json:"memberName"`
	Email      string `json:"email"`
	BookID     int64  `json:"bookId"`
	Title      string `json:"title"`
	Position   int    `json:"position"`
}

type HoldReadyData struct {
	HoldID     int64     `json:"holdId"`
	MemberID   int64     `json:"memberId"`
	MemberName string    `json:"memberName"`
	Email      string    `json:"email"`
	BookID     int64     `json:"bookId"`
	CopyID     int64     `json:"copyId"`
	Title      string    `json:"title"`
	ExpiresAt  time.Time `json:"expiresAt"`
}
