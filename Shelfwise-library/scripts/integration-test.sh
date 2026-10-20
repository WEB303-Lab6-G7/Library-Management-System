#!/usr/bin/env bash
# Part G — integration and system tests.
# Runs every required scenario against the running stack and writes test-results.md.
#
#   ./scripts/integration-test.sh
#
# Needs: curl, jq, and the stack running (docker compose up -d).
# Signs in as the seeded librarian (LIBRARIAN_EMAIL / LIBRARIAN_PASSWORD) and creates test members.
# Stopping/starting consumers uses docker compose by default; override with
#   STOP_CMD='my-stop %s' START_CMD='my-start %s'
set -uo pipefail

API=${API:-http://localhost:8090/api}
STOP_CMD=${STOP_CMD:-"docker compose stop %s"}
START_CMD=${START_CMD:-"docker compose start %s"}
OUT=${OUT:-test-results.md}
RUN=$(date +%s)   # makes emails unique so the script can be re-run

pass=0; fail=0; rows=()
c_green=$'\e[32m'; c_red=$'\e[31m'; c_dim=$'\e[2m'; c_off=$'\e[0m'

record() { # id, scenario, expected, actual, ok(0/1)
  if [ "$5" = 0 ]; then pass=$((pass+1)); res="PASS"; col=$c_green; else fail=$((fail+1)); res="FAIL"; col=$c_red; fi
  printf '%s%-4s%s %-3s %s\n     %sexpected:%s %s\n     %sactual:  %s %s\n' "$col" "$res" "$c_off" "$1" "$2" "$c_dim" "$c_off" "$3" "$c_dim" "$c_off" "$4"
  rows+=("| $1 | $2 | $3 | $4 | **$res** |")
}
# Every request runs as a signed-in user: JAR selects whose session cookie is used.
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
JAR_LIB=$TMP/librarian; JAR_A=$TMP/a; JAR_B=$TMP/b; JAR_C=$TMP/c
get()  { curl -s -b "${JAR:-$JAR_LIB}" "$API$1"; }
post() { curl -s -b "${JAR:-$JAR_LIB}" -c "${JAR:-$JAR_LIB}" -X POST -H 'Content-Type: application/json' -d "${2:-{\}}" "$API$1"; }
code() { curl -s -o /dev/null -w '%{http_code}' -b "${JAR:-$JAR_LIB}" -X POST -H 'Content-Type: application/json' -d "$2" "$API$1"; }
svc_stop()  { eval "${STOP_CMD//%s/$1}"  >/dev/null 2>&1; }
svc_start() { eval "${START_CMD//%s/$1}" >/dev/null 2>&1; }
# wait_for "<description>" <seconds> <command...>: retry until the command succeeds
wait_for() { local t=$2; shift 2; for ((i=0;i<t*2;i++)); do if eval "$@" >/dev/null 2>&1; then return 0; fi; sleep 0.5; done; return 1; }
queue_ready() { get /system/status | jq -r --arg q "$1" '.queues[] | select(.name==$q) | .messages_ready'; }
event_seen() { get "/system/events?limit=200" | jq -e --arg t "$1" --argjson k "$2" --arg f "$3" \
  '[.[] | select(.eventType==$t and (.data[$f]|tostring)==($k|tostring))] | length > 0'; }
member_status() { get /members | jq -r ".[] | select(.id==$1) | .status"; }
in_view() { get /member-view | jq -e ".[] | select(.memberId==$1${2:+ and .status==\"$2\"})"; }

echo "Running integration tests against $API"
echo

# ---------- setup ----------
LIB_EMAIL=${LIBRARIAN_EMAIL:-librarian@library.bt}; LIB_PASS=${LIBRARIAN_PASSWORD:-librarian123}
JAR=$JAR_LIB post /auth/login "{\"email\":\"$LIB_EMAIL\",\"password\":\"$LIB_PASS\"}" | jq -e '.role=="librarian"' >/dev/null \
  || { echo "Could not sign in as the librarian ($LIB_EMAIL). Is the stack running?"; exit 1; }
reg() { JAR=$1 post /auth/register "{\"name\":\"$2 $RUN\",\"email\":\"$3.$RUN@test.bt\",\"password\":\"secret123\"}"; }
A_JSON=$(reg "$JAR_A" "Yeshey Test" yeshey); A=$(jq .id <<<"$A_JSON")
B=$(reg "$JAR_B" "Ndup Test" ndup | jq .id)
C=$(reg "$JAR_C" "Sonam Test" sonam | jq .id)
BOOK=$(post /books "{\"title\":\"Test Book One $RUN\",\"author\":\"Test Author\",\"category\":\"Testing\",\"copies\":1}" | jq .id)
BOOK2=$(post /books "{\"title\":\"Test Book Two $RUN\",\"author\":\"Test Author\",\"category\":\"Testing\",\"copies\":3}" | jq .id)
sleep 2   # let every service process the MemberRegistered events

# ---------- T1 Successful business operation ----------
ok=1; wait_for "" 10 "event_seen MemberRegistered $A memberId" && [ -s "$JAR_A" ] && ok=0
record T1 "Successful business operation" "Create account → 201, session cookie issued, MemberRegistered published" \
  "member #$A created (role $(jq -r .role <<<"$A_JSON")); MemberRegistered $( [ $ok = 0 ] && echo seen on bus || echo NOT seen )" $ok

# ---------- T2 Event delivery ----------
ok=1; wait_for "" 10 "in_view $A" && ok=0
record T2 "Event delivery" "Circulation consumes MemberRegistered and adds the member to its local view" \
  "member #$A $( [ $ok = 0 ] && echo present || echo missing ) in circulation member_view" $ok
wait_for "" 10 "in_view $B"; wait_for "" 10 "in_view $C"

# ---------- T3 Multiple consumers ----------
LOAN1=$(JAR=$JAR_A post /loans "{\"bookId\":$BOOK}" | jq .id)
wait_for "" 10 "get /books/$BOOK | jq -e '.availableCopies==0'"; cat_ok=$?
wait_for "" 10 "JAR=$JAR_A get /notifications | jq -e '.[] | select(.eventType==\"BookBorrowed\")'"; not_ok=$?
ok=$(( cat_ok || not_ok ))
record T3 "Multiple consumers" "Member borrows → BookBorrowed processed by Catalog (copy on loan) AND Notification (receipt)" \
  "catalog available=$(get /books/$BOOK | jq .availableCopies); receipt notification $( [ $not_ok = 0 ] && echo sent || echo missing )" $ok

# ---------- T4 Event chaining ----------
HOLD=$(JAR=$JAR_B post /holds "{\"bookId\":$BOOK}" | jq .holdId)
JAR=$JAR_A post /loans/$LOAN1/return >/dev/null
wait_for "" 15 "get /holds | jq -e '.[] | select(.id==$HOLD and .status==\"ready\")'"; h_ok=$?
wait_for "" 15 "get /books/$BOOK | jq -e '.onHold==1'"; c_ok=$?
ok=$(( h_ok || c_ok ))
record T4 "Event chaining" "BookReturned → Reservation publishes HoldReady → Catalog sets copy aside + member notified" \
  "hold #$HOLD status=$(get /holds | jq -r ".[] | select(.id==$HOLD) | .status"); catalog onHold=$(get /books/$BOOK | jq .onHold)" $ok

# Chain 2: late return → FineIssued → MemberSuspended → Circulation blocks borrowing
LOAN2=$(post /loans "{\"memberId\":$A,\"bookId\":$BOOK2,\"dueInDays\":-8}" | jq .id)
post /loans/$LOAN2/return >/dev/null
wait_for "" 15 "in_view $A suspended"
blocked=$(JAR=$JAR_A code /loans "{\"bookId\":$BOOK2}")
ok=1; [ "$blocked" = 403 ] && ok=0
record T4b "Event chaining (3 hops)" "Late return → FineIssued (Nu.80) → MemberSuspended → member's borrow blocked (403)" \
  "fine=$(get "/fines?memberId=$A" | jq '.[0].amount'); member=$(member_status $A); borrow attempt → HTTP $blocked" $ok

# ---------- T5 Consumer unavailable ----------
svc_stop fine
svc_stop notification
sleep 2
LOAN3=$(post /loans "{\"memberId\":$C,\"bookId\":$BOOK2,\"dueInDays\":-3}" | jq .id)
rcode=$(code /loans/$LOAN3/return '{}')
wait_for "" 12 "[ \"\$(queue_ready fine.events)\" -ge 1 ]"; q_ok=$?
depth=$(queue_ready fine.events)
ok=$(( q_ok )); [ "$rcode" = 200 ] || ok=1
record T5 "Consumer unavailable" "Fine + Notification stopped; loan/return still succeed and BookReturned waits in the durable queue" \
  "return → HTTP $rcode; fine.events holds $depth message(s); fine service → HTTP $(curl -s -o /dev/null -w '%{http_code}' -b $JAR_LIB "$API/fines")" $ok

# ---------- T6 Consumer recovery ----------
svc_start fine
svc_start notification
wait_for "" 40 "get /fines | jq -e '.[] | select(.loanId==$LOAN3)'"; f_ok=$?
wait_for "" 20 "[ \"\$(queue_ready fine.events)\" = 0 ]"
ok=$f_ok
record T6 "Consumer recovery" "After restart, Fine processes the pending BookReturned and issues the fine" \
  "fine for loan #$LOAN3: $(get /fines | jq -r "[.[]|select(.loanId==$LOAN3)][0].amount // \"none\""); fine.events depth now $(queue_ready fine.events)" $ok

# ---------- T7 Duplicate event ----------
wait_for "" 20 "get '/system/events?limit=200' | jq -e '.[] | select(.eventType==\"BookReturned\" and .data.loanId==$LOAN2)'"
EV=$(get "/system/events?limit=200" | jq -c "[.[] | select(.eventType==\"BookReturned\" and .data.loanId==$LOAN2)][0] | del(.receivedAt, .duplicates)")
before_f=$(get /fines | jq "[.[]|select(.loanId==$LOAN2)]|length")
before_n=$(JAR=$JAR_A get /notifications | jq length)
post /system/replay "$EV" >/dev/null; post /system/replay "$EV" >/dev/null
sleep 4
after_f=$(get /fines | jq "[.[]|select(.loanId==$LOAN2)]|length")
after_n=$(JAR=$JAR_A get /notifications | jq length)
dups=$(get "/system/events?limit=200" | jq "[.[] | select(.eventId==$(jq '.eventId' <<<"$EV"))][0].duplicates")
ok=1; [ "$before_f" = "$after_f" ] && [ "$before_n" = "$after_n" ] && [ "${dups:-0}" -ge 2 ] && ok=0
record T7 "Duplicate event" "Same BookReturned delivered twice more → no extra fine, no extra notification" \
  "fines for loan: $before_f → $after_f; notifications: $before_n → $after_n; duplicates detected: $dups" $ok

# ---------- T8 Processing error → retries → DLQ ----------
before_dlq=$(queue_ready fine.events.dlq)
post /system/failure-mode '{"enabled":true}' >/dev/null
LOAN4=$(post /loans "{\"memberId\":$C,\"bookId\":$BOOK2,\"dueInDays\":-1}" | jq .id)
post /loans/$LOAN4/return >/dev/null
wait_for "" 40 "[ \"\$(queue_ready fine.events.dlq)\" -gt $before_dlq ]"; ok=$?
post /system/failure-mode '{"enabled":false}' >/dev/null
record T8 "Message-processing error" "Fine handler fails → 3 retries (5 s apart) → message parked in fine.events.dlq, not lost" \
  "fine.events.dlq: $before_dlq → $(queue_ready fine.events.dlq)" $ok

# ---------- T9 Complete user workflow ----------
for f in $(JAR=$JAR_A get /fines | jq '.[] | select(.status=="unpaid") | .id'); do JAR=$JAR_A post /fines/$f/pay >/dev/null; done
wait_for "" 15 "in_view $A active"
again=$(JAR=$JAR_A code /loans "{\"bookId\":$BOOK2}")
ok=1; [ "$again" = 201 ] && ok=0
record T9 "Complete user workflow" "Member pays fine → FinePaid → MemberReactivated → Circulation allows borrowing again" \
  "member=$(member_status $A); borrow attempt → HTTP $again" $ok

# ---------- T10 Authentication and access control ----------
bad=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "{\"email\":\"$LIB_EMAIL\",\"password\":\"wrong-password\"}" "$API/auth/login")
anon=$(curl -s -o /dev/null -w '%{http_code}' "$API/loans")
forbidden=$(JAR=$JAR_A code /books '{"title":"x","author":"y"}')
other=$(JAR=$JAR_B code /loans/$LOAN2/return '{}')
scoped=$(JAR=$JAR_B get /loans | jq "[.[] | select(.memberId!=$B)] | length")
ok=1; [ "$bad" = 401 ] && [ "$anon" = 401 ] && [ "$forbidden" = 403 ] && [ "$other" = 404 ] && [ "$scoped" = 0 ] && ok=0
record T10 "Authentication & access control" "Wrong password 401; no session 401; member adding a book 403; member returning someone else's loan 404; members only see their own loans" \
  "wrong password → $bad; no session → $anon; member POST /books → $forbidden; other member's loan → $other; foreign loans visible: $scoped" $ok

# ---------- report ----------
{
  echo "# Integration test results"
  echo
  echo "Run: $(date '+%Y-%m-%d %H:%M:%S')  ·  Passed: $pass  ·  Failed: $fail"
  echo
  echo "| # | Scenario | Expected | Actual | Result |"
  echo "|---|---|---|---|---|"
  printf '%s\n' "${rows[@]}"
} > "$OUT"
echo
echo "Passed $pass, failed $fail. Results written to $OUT"
[ "$fail" = 0 ]
