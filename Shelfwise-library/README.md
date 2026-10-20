# Library Management System: Event-Driven Microservices

WEB303 Lab 6 · Yeshey Zhennue · Ndup Dorji · Sonam Zangmo

**Shelfwise** is a library website built from six independent Go microservices that
communicate asynchronously through **RabbitMQ**. Members create an account, borrow and
return books, join a waitlist for books that are out, get fined for late returns, and are
automatically suspended or reactivated, all driven by events. Librarians manage the
catalogue, issue loans at the desk, and watch the system live.

## Quick start

```bash
docker compose up -d --build        # first build takes a few minutes
open http://localhost:8090          # website
open http://localhost:15672         # RabbitMQ dashboard (guest / guest)
./scripts/integration-test.sh       # Part G tests → test-results.md (needs curl + jq)
go test ./...                       # unit tests (no services needed)
```

Stop everything with `docker compose down` (add `-v` to wipe all data).

**Ports on your machine:** website 8090, services 8081–8086, RabbitMQ 5672 and 15672. PostgreSQL is not exposed, so it never clashes with a local Postgres. If a port is already in use, change the number on the left of the colon in `docker-compose.yml`.

**Accounts:** create a member account on the website, or sign in as staff with
`librarian@library.bt` / `librarian123` (set in `docker-compose.yml`). The catalogue is
seeded with 10 starter books on first start.

## The website

| Area | Pages |
|---|---|
| Public | Home `/`, Catalogue `/catalog`, Book details `/books/:id`, one landing page per feature `/features/{catalogue, borrowing, holds, fines, notifications, librarians}`, Sign in `/login`, Create account `/register` |
| Member | Dashboard `/dashboard`, My loans `/loans`, Holds `/holds`, Fines `/fines`, Notifications `/notifications`, Profile `/account` |
| Librarian | Overview `/admin`, Catalogue, Members (with Circulation sync column), Circulation (issue loans, returns), Holds, Fines (record payments), Notifications, System monitor `/admin/system` |

The website is a single-page app (`gateway/web`) served by the gateway, with clean URLs,
line icons, and layouts for both desktop and mobile.

### Authentication and access control

- The **Member service** owns credentials. Passwords are stored only as salted PBKDF2-SHA256 hashes, and `POST /auth/login` verifies them.
- The **API Gateway** owns sessions. After a successful login it issues a session token signed with HMAC-SHA256, so the browser cannot change its own user id or role.
- The website keeps that token **per browser tab** and sends it as `Authorization: Bearer <token>`, so one tab can be signed in as the librarian and another as a member at the same time. (The token is also set as an `HttpOnly` cookie, used only by clients that send no header, such as the test script.)
- The gateway checks every API call against the user's role:
  - Visitors can browse the catalogue.
  - Members only see and act on their **own** loans, holds, fines and notifications. The gateway forces their member id, and checks ownership before a return or payment.
  - Librarian-only endpoints, such as adding books, issuing loans with a custom period, the member list and the system monitor, return `403` to anyone else.
- The services themselves stay unchanged and trust the gateway, which is the only public entry point.

| Gateway API | Who |
|---|---|
| `POST /api/auth/register`, `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/auth/me` | anyone |
| `GET /api/books`, `GET /api/books/{id}` | anyone |
| `POST /api/books` | librarian |
| `GET/POST /api/loans`, `POST /api/loans/{id}/return` | member (own) / librarian (all) |
| `GET/POST /api/holds` | member (own) / librarian (all) |
| `GET /api/fines`, `POST /api/fines/{id}/pay` | member (own) / librarian (all) |
| `GET /api/notifications` | member (own) / librarian (all) |
| `GET /api/members`, `GET /api/member-view`, `/api/system/*` | librarian |

## Architecture

```mermaid
flowchart LR
    UI[Website] --> GW["API Gateway :8090<br/>sessions + access control"]
    GW --> CAT[Catalog :8081]
    GW --> MEM[Member :8082]
    GW --> CIR[Circulation :8083]
    GW --> RES[Reservation :8084]
    GW --> FIN[Fine :8085]
    GW --> NOT[Notification :8086]

    CIR -. "REST: available copy?" .-> CAT
    RES -. "REST: book on shelf?" .-> CAT

    subgraph RabbitMQ["RabbitMQ · topic exchange library.events"]
      EX((library.events))
    end

    MEM -- MemberRegistered / Suspended / Reactivated --> EX
    CAT -- BookAdded --> EX
    CIR -- BookBorrowed / BookReturned / BookOverdue --> EX
    FIN -- FineIssued / FinePaid --> EX
    RES -- HoldPlaced / HoldReady --> EX

    EX -- BookBorrowed, BookReturned, HoldReady --> CAT
    EX -- FineIssued, FinePaid --> MEM
    EX -- MemberRegistered, Suspended, Reactivated --> CIR
    EX -- MemberRegistered, BookReturned, BookBorrowed --> RES
    EX -- BookOverdue, BookReturned --> FIN
    EX -- "all events (#)" --> NOT
```

Each service has its **own PostgreSQL database** (`catalog_db`, `member_db`, …). No
service reads another's database. Data a service needs from elsewhere arrives in events.
For example, Circulation keeps its own `member_view` table, built only from Member events.

| Service | Responsibility | Publishes | Consumes |
|---|---|---|---|
| **Catalog** | Books and physical copies | BookAdded | BookBorrowed, BookReturned, HoldReady |
| **Member** | Members, credentials and roles, status, outstanding fines | MemberRegistered, MemberSuspended, MemberReactivated | FineIssued, FinePaid |
| **Circulation** | Borrow, return, overdue detection | BookBorrowed, BookReturned, BookOverdue | MemberRegistered, MemberSuspended, MemberReactivated |
| **Reservation** | Hold queue (waitlist) | HoldPlaced, HoldReady | MemberRegistered, BookReturned, BookBorrowed |
| **Fine** | Late fees and payments | FineIssued, FinePaid | BookOverdue, BookReturned |
| **Notification** | Messages to members and an audit log of all events | — | every event |
| **Gateway** | Single entry point: website, sessions, access control, queue monitor, event replay | — | — |

## Events

Every event travels in the same envelope:

```json
{
  "eventId": "6f1c…",            // unique: used for duplicate detection
  "eventType": "BookReturned",   // also the RabbitMQ routing key
  "source": "circulation",
  "occurredAt": "2026-10-15T05:30:00Z",
  "data": { "loanId": 42, "memberId": 7, "bookId": 15, "copyId": 31, "title": "…",
            "dueDate": "…", "returnedAt": "…", "daysLate": 5, "memberName": "…", "email": "…" }
}
```

Payloads carry everything consumers need (names, emails, titles, `daysLate`), so no
consumer has to call the publisher back. All payloads are defined in `pkg/events/event.go`.

| Event | Purpose |
|---|---|
| MemberRegistered | A new member joined; other services add them to their local views |
| MemberSuspended / MemberReactivated | Borrowing privileges changed because of fines |
| BookAdded | New title with N copies added to the catalogue |
| BookBorrowed | A copy was lent; Catalog marks it on loan, Reservation closes any hold |
| BookReturned | A copy came back; Catalog frees it, Fine charges if late, Reservation serves the queue |
| BookOverdue | The overdue checker found a loan past its due date |
| FineIssued / FinePaid | Fine created or paid; carries the member's total outstanding balance |
| HoldPlaced | Member joined the waitlist; carries their queue position |
| HoldReady | A returned copy is set aside for the first person in the queue |

### Required patterns

- **Multiple consumers:** `BookReturned` is processed by Catalog, Fine, Reservation and Notification. `BookBorrowed` is processed by Catalog, Reservation and Notification.
- **Event chaining:**
  1. `BookReturned` → Reservation → `HoldReady` → Catalog sets the copy aside, and Notification sends "ready for pickup"
  2. `BookReturned` (late) → Fine → `FineIssued` → Member → `MemberSuspended` → Circulation blocks borrowing
  3. `FinePaid` → Member → `MemberReactivated` → Circulation allows borrowing again

## Reliability (Part E)

| Concern | Mechanism | Where |
|---|---|---|
| Message loss on broker restart | Durable exchange and queues, persistent messages | `pkg/events/broker.go` |
| Message loss on publish | Publisher confirms, plus a **transactional outbox**: the event is saved in the same DB transaction as the data change and relayed to RabbitMQ afterwards | `pkg/store/store.go` |
| Consumer down | Durable per-service queues keep messages until the consumer returns | `Subscribe` |
| Processing errors | Manual acks; failures go to `<queue>.retry` (5 s delay), up to 3 retries, then `<queue>.dlq` | `process` / `forward` |
| Duplicate delivery | Each consumer records `eventId` in `processed_events` in the same transaction as its work; repeats are ignored | `store.Idempotent` |
| Logging | Every publish, consume, retry, DLQ move and duplicate is logged | `docker compose logs -f <service>` |
| Restart | Services reconnect on startup, and RabbitMQ redelivers anything un-acked | `events.Connect` |

## Demo script (Part I)

Open several tabs at http://localhost:8090: one signed in as the **librarian** and one per **member**. Each tab keeps its own login.

1. **Create an account** on `/register`. On the librarian's *Members* page the new member appears, and the *Circulation sync* column changes from *Pending* to *In sync* as the `MemberRegistered` event is consumed.
2. **Borrow** a book from its page. Availability drops and a receipt arrives under *Notifications*. That's one event with two consumers.
3. As a second member, open a book with no copies left and click **Join the waitlist**. Return the first member's loan: the hold becomes *Ready for pickup* and the copy shows as *Set aside* (event chain).
4. As the librarian, go to *Circulation* and **Issue loan** with a period of **−8** days, then **Return** it. A Nu. 80 fine is issued, the member is **suspended**, and their dashboard shows that borrowing is paused. The member pays on *Fines* and borrowing is restored.
5. **Failure and recovery:** `docker compose stop fine notification`, then return a late book. The website keeps working, and the *System monitor* shows the services as down with messages waiting. `docker compose start fine notification` processes the backlog.
6. **Retries and DLQ:** on the *System monitor*, switch on *Simulate processing failures* and return a late book. Watch the *Retry* column, then *DLQ* (about 15 s). Switch it off afterwards.
7. **Duplicates:** click **Replay** on any event in the stream. Its "duplicates ignored" counter rises, and no extra fine or notification is created.

## Configuration

| Variable | Default | Service |
|---|---|---|
| `LOAN_DAYS` | 14 | circulation |
| `OVERDUE_CHECK_SECONDS` | 20 | circulation |
| `FINE_PER_DAY` | 10 (Nu.) | fine |
| `FINE_SUSPEND_THRESHOLD` | 50 (Nu.) | member |
| `LIBRARIAN_EMAIL` / `LIBRARIAN_PASSWORD` | librarian@library.bt / librarian123 | member |
| `SEED_BOOKS` | true | catalog |
| `SESSION_SECRET` | set in docker-compose.yml (random if empty) | gateway |

## Project layout

```
pkg/events/       event contract + RabbitMQ publish/subscribe, retry, DLQ
pkg/store/        DB connect, idempotent handler wrapper, transactional outbox
pkg/svc/          HTTP helpers
services/<name>/  one main.go per microservice
gateway/          API gateway: routing, sessions (session.go), website (web/)
deploy/           Postgres init script (one database per service)
scripts/          integration-test.sh (Part G)
```

## Running one service on its own

Each service is independently startable and testable. For example, with Postgres and
RabbitMQ running:

```bash
# temporarily add  ports: ["5434:5432"]  under postgres in docker-compose.yml, then:
docker compose up -d postgres rabbitmq
DATABASE_URL=postgres://library:library@localhost:5434/fine_db?sslmode=disable go run ./services/fine
curl localhost:8085/health
```

## Work division

| Member | Owns |
|---|---|
| Yeshey Zhennue | Catalog, Reservation, `pkg/events` (broker, event contract), RabbitMQ setup |
| Ndup Dorji | Circulation, Fine, overdue checker, reliability (outbox, retries, DLQ, idempotency) |
| Sonam Zangmo | Member (incl. passwords and login), Notification, Gateway (sessions, access control), website, integration tests |