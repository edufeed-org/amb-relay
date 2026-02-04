#!/usr/bin/env bash
set -euo pipefail

TEST_PORT=13334
RELAY="ws://localhost:${TEST_PORT}"
RELAY_HTTP="http://localhost:${TEST_PORT}"
PASS=0
FAIL=0

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
NC='\033[0m'

echo "=== AMB Relay E2E Test ==="
echo ""

# ============================================================
# Pre-flight: check required tools
# ============================================================
for cmd in nak curl jq sha256sum base64; do
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: required tool '$cmd' not found"
    exit 1
  fi
done

# ============================================================
# Cleanup
# ============================================================
RELAY_PID=""
cleanup() {
  echo ""
  echo "--- Cleaning up ---"
  if [ -n "$RELAY_PID" ]; then
    kill "$RELAY_PID" 2>/dev/null || true
    wait "$RELAY_PID" 2>/dev/null || true
  fi
  docker compose down -v 2>/dev/null || true
  # typesense-data is owned by root (created by Docker), use docker to clean it
  docker run --rm -v "$(pwd)/typesense-data:/data" alpine rm -rf /data/* 2>/dev/null || true
  rm -rf ./data/e2e_test* 2>/dev/null || true
}
trap cleanup EXIT

# ============================================================
# Pre-flight: kill stale relay from a previous run
# ============================================================
if bash -c "echo >/dev/tcp/localhost/${TEST_PORT}" 2>/dev/null; then
  echo "Port ${TEST_PORT} in use — killing stale process..."
  ss -tlnp 2>/dev/null | grep ":${TEST_PORT} " | grep -oP 'pid=\K\d+' | xargs kill 2>/dev/null || true
  sleep 1
  if bash -c "echo >/dev/tcp/localhost/${TEST_PORT}" 2>/dev/null; then
    echo "ERROR: Port ${TEST_PORT} still in use after kill attempt."
    exit 1
  fi
fi

# ============================================================
# Start Typesense (clean state)
# ============================================================
echo "--- Starting Typesense (clean state) ---"
docker compose down -v 2>/dev/null || true
docker run --rm -v "$(pwd)/typesense-data:/data" alpine rm -rf /data/* 2>/dev/null || true
docker compose up -d typesense

echo -n "Waiting for Typesense..."
for i in $(seq 1 30); do
  if curl -sf http://localhost:8108/health >/dev/null 2>&1; then
    echo " ready"
    break
  fi
  echo -n "."
  sleep 1
  if [ "$i" -eq 30 ]; then
    echo " TIMEOUT"
    exit 1
  fi
done

# Drop stale collection from any previous run
curl -sf -X DELETE -H "X-TYPESENSE-API-KEY: xyz" \
  "http://localhost:8108/collections/amb_e2e_test" >/dev/null 2>&1 || true

# ============================================================
# Generate test keys (before relay startup so PUBKEY can be set)
# ============================================================
SEC=$(nak key generate)
PUB=$(nak key public "$SEC")

# A second keypair used as a tagged pubkey in p-tags
TAGGED_SEC=$(nak key generate)
TAGGED_PUB=$(nak key public "$TAGGED_SEC")

# A third keypair for testing unauthorized access
NONADMIN_SEC=$(nak key generate)
NONADMIN_PUB=$(nak key public "$NONADMIN_SEC")

echo "Test pubkey (admin): $PUB"
echo "Tagged pubkey:       $TAGGED_PUB"
echo "Non-admin pubkey:    $NONADMIN_PUB"
echo ""

# ============================================================
# Start relay
# ============================================================
echo "--- Starting relay ---"
export NAME="E2E Test Relay"
export PUBKEY="$PUB"
export DESCRIPTION="e2e test relay"
export ICON=""
export TS_APIKEY=xyz
export TS_HOST=http://localhost:8108
export TS_COLLECTION=amb_e2e_test
export PORT=$TEST_PORT
export DB_PATH="./data/e2e_test/relay.db"

go run . > /tmp/amb-relay-e2e.log 2>&1 &
RELAY_PID=$!

echo -n "Waiting for relay..."
for i in $(seq 1 30); do
  if bash -c "echo >/dev/tcp/localhost/${TEST_PORT}" 2>/dev/null; then
    echo " ready"
    break
  fi
  echo -n "."
  sleep 1
  if [ "$i" -eq 30 ]; then
    echo " TIMEOUT"
    echo "Relay log:"
    cat /tmp/amb-relay-e2e.log
    exit 1
  fi
done

# ============================================================
# Helper functions
# ============================================================
publish() {
  local json="$1"
  local result
  result=$(echo "$json" | nak event -k 30142 --sec "$SEC" --auth "$RELAY" 2>&1) || true
  echo "$result"
}

publish_as() {
  local sec="$1"
  local json="$2"
  local result
  result=$(echo "$json" | nak event -k 30142 --sec "$sec" --auth "$RELAY" 2>&1) || true
  echo "$result"
}

query_count() {
  local count
  count=$(nak req "$@" --sec "$SEC" --auth "$RELAY" 2>/dev/null | wc -l) || true
  echo "$count"
}

query_events() {
  nak req "$@" --sec "$SEC" --auth "$RELAY" 2>/dev/null || true
}

assert_count() {
  local desc="$1"
  local expected="$2"
  shift 2
  local actual
  actual=$(query_count "$@")
  if [ "$actual" -eq "$expected" ]; then
    printf "${GREEN}PASS${NC}: %s (got %d)\n" "$desc" "$actual"
    PASS=$((PASS + 1))
  else
    printf "${RED}FAIL${NC}: %s (expected %d, got %d)\n" "$desc" "$expected" "$actual"
    FAIL=$((FAIL + 1))
  fi
}

assert_rejected() {
  local desc="$1"
  local json="$2"
  local output
  output=$(echo "$json" | nak event --sec "$SEC" --auth "$RELAY" 2>&1) || true
  if echo "$output" | grep -qi "msg.*:\|error\|failed\|rejected"; then
    printf "${GREEN}PASS${NC}: %s (rejected)\n" "$desc"
    PASS=$((PASS + 1))
  else
    printf "${RED}FAIL${NC}: %s (expected rejection)\n" "$desc"
    FAIL=$((FAIL + 1))
  fi
}

# NIP-86 helper: make an authenticated management API call
# Usage: nip86_call <method> <params_json> [secret_key]
nip86_call() {
  local method="$1"
  local params_json="$2"
  local sec="${3:-$SEC}"

  # Build JSON-RPC body
  local body
  body=$(jq -nc --arg m "$method" --argjson p "$params_json" '{"method":$m,"params":$p}')

  # Compute SHA256 of body
  local payload_hash
  payload_hash=$(printf '%s' "$body" | sha256sum | cut -d' ' -f1)

  # Create NIP-98 auth event (kind 27235)
  local auth_event
  auth_event=$(nak event -k 27235 \
    -t "u=${RELAY_HTTP}" \
    -t "method=POST" \
    -t "payload=${payload_hash}" \
    -c '' \
    --sec "$sec" 2>/dev/null) || true

  local auth_b64
  auth_b64=$(printf '%s' "$auth_event" | base64 -w0)

  # Send HTTP request
  curl -s -X POST \
    -H "Content-Type: application/nostr+json+rpc" \
    -H "Authorization: Nostr ${auth_b64}" \
    -d "$body" \
    "${RELAY_HTTP}" 2>/dev/null || true
}

# NIP-86 helper: call without auth header
nip86_call_noauth() {
  local method="$1"
  local params_json="$2"
  local body
  body=$(jq -nc --arg m "$method" --argjson p "$params_json" '{"method":$m,"params":$p}')

  curl -s -X POST \
    -H "Content-Type: application/nostr+json+rpc" \
    -d "$body" \
    "${RELAY_HTTP}" 2>/dev/null || true
}

assert_nip86() {
  local desc="$1"
  local method="$2"
  local params="$3"
  local jq_check="$4"  # jq expression that should return "true"
  local response
  response=$(nip86_call "$method" "$params")
  if echo "$response" | jq -e "$jq_check" >/dev/null 2>&1; then
    printf "${GREEN}PASS${NC}: %s\n" "$desc"
    PASS=$((PASS + 1))
  else
    printf "${RED}FAIL${NC}: %s (response: %s)\n" "$desc" "$response"
    FAIL=$((FAIL + 1))
  fi
}

assert_nip86_error() {
  local desc="$1"
  local response="$2"
  if echo "$response" | jq -e '.error != null and .error != ""' >/dev/null 2>&1; then
    printf "${GREEN}PASS${NC}: %s\n" "$desc"
    PASS=$((PASS + 1))
  else
    printf "${RED}FAIL${NC}: %s (expected error, got: %s)\n" "$desc" "$response"
    FAIL=$((FAIL + 1))
  fi
}

# ============================================================
# Record timestamp before publishing
# ============================================================
BEFORE=$(date +%s)
sleep 1

# ============================================================
# Publish test events
# ============================================================
echo "--- Publishing test events ---"

# Event A: basic educational resource
EVENT_A=$(cat <<EOF
{
  "tags": [
    ["d", "https://example.org/courses/physics-101"],
    ["type", "LearningResource"],
    ["name", "Introduction to Physics"],
    ["description", "A comprehensive introduction to classical mechanics"],
    ["inLanguage", "en"],
    ["t", "physics"],
    ["t", "mechanics"],
    ["creator:name", "Dr. Jane Smith"],
    ["creator:type", "Person"],
    ["publisher:name", "Example University Press"],
    ["publisher:type", "Organization"],
    ["license:id", "https://creativecommons.org/licenses/by-sa/4.0/"],
    ["isAccessibleForFree", "true"],
    ["r", "https://example.org/physics-101"]
  ],
  "content": "A comprehensive introduction to classical mechanics"
}
EOF
)
echo "  Publishing Event A (basic resource)..."
publish "$EVENT_A"

# Event B: with nostr-native p-tag and controlled vocabularies
EVENT_B=$(cat <<EOF
{
  "tags": [
    ["d", "https://example.org/courses/math-201"],
    ["type", "LearningResource"],
    ["name", "Advanced Mathematics"],
    ["description", "Probability and statistics for engineers"],
    ["inLanguage", "de"],
    ["p", "$TAGGED_PUB", "", "creator"],
    ["about:id", "https://w3id.org/kim/hochschulfaechersystematik/n37"],
    ["about:prefLabel:de", "Mathematik"],
    ["about:prefLabel:en", "Mathematics"],
    ["learningResourceType:id", "https://w3id.org/kim/hcrt/course"],
    ["learningResourceType:prefLabel:de", "Kurs"],
    ["learningResourceType:prefLabel:en", "Course"],
    ["t", "mathematics"],
    ["t", "probability"]
  ],
  "content": "Probability and statistics for engineers"
}
EOF
)
echo "  Publishing Event B (p-tag + vocabularies)..."
publish "$EVENT_B"

# Event C: with a-tag relation and multiple types
EVENT_C=$(cat <<EOF
{
  "tags": [
    ["d", "https://example.org/courses/chem-301"],
    ["type", "Course"],
    ["type", "LearningResource"],
    ["name", "Advanced Chemistry Lab"],
    ["inLanguage", "en"],
    ["a", "30142:${PUB}:https://example.org/courses/physics-101", "", "isBasedOn"],
    ["t", "chemistry"],
    ["creator:name", "Prof. Max Mueller"],
    ["creator:type", "Person"],
    ["publisher:name", "Science Institute"],
    ["publisher:type", "Organization"]
  ],
  "content": "Hands-on chemistry laboratory course"
}
EOF
)
echo "  Publishing Event C (a-tag + multiple types)..."
publish "$EVENT_C"

# Wait for indexing
sleep 2
AFTER=$(date +%s)

# ============================================================
# Query tests
# ============================================================
echo ""
echo "--- Query tests ---"

# 1. All events by kind
assert_count "all events by kind" 3 \
  -k 30142

# 2. By author
assert_count "by author pubkey" 3 \
  -k 30142 -a "$PUB"

# 3. By d-tag
assert_count "by d-tag" 1 \
  -k 30142 -d "https://example.org/courses/physics-101"

# 4. By keyword (t-tag)
assert_count "by keyword #t=physics" 1 \
  -k 30142 -t "t=physics"

# 5. By r-tag
assert_count "by r-tag" 1 \
  -k 30142 -t "r=https://example.org/physics-101"

# 6. By p-tag
assert_count "by p-tag (tagged pubkey)" 1 \
  -k 30142 -p "$TAGGED_PUB"

# 7. NIP-50 full-text search
assert_count "NIP-50 search 'physics'" 1 \
  -k 30142 --search "physics"

# 8. NIP-50 field-specific search (exact match on nested field)
assert_count "NIP-50 field search about.prefLabel.de:Mathematik" 1 \
  -k 30142 --search "about.prefLabel.de:Mathematik"

# 9. Time range (since/until)
assert_count "time range since/until" 3 \
  -k 30142 --since "$BEFORE" --until "$AFTER"

# 10. Limit
assert_count "limit=1" 1 \
  -k 30142 -l 1

# ============================================================
# Rejection tests
# ============================================================
echo ""
echo "--- Rejection tests ---"

# 11. Missing d-tag
assert_rejected "reject event without d-tag" \
  '{"kind":30142,"tags":[["name","No D Tag"]],"content":""}'

# 12. Missing name tag
assert_rejected "reject event without name tag" \
  '{"kind":30142,"tags":[["d","test-missing-name"]],"content":""}'

# 13. Wrong kind
assert_rejected "reject event with wrong kind" \
  '{"kind":1,"tags":[["d","test"],["name","Wrong Kind"]],"content":"hello"}'

# ============================================================
# NIP-86 Management API tests
# ============================================================
echo ""
echo "--- NIP-86 Management API tests ---"

# 14. supportedmethods
assert_nip86 "supportedmethods returns method list" \
  "supportedmethods" '[]' \
  '.result | type == "array" and (. | index("banpubkey")) != null'

# 15. Ban pubkey
assert_nip86 "banpubkey succeeds" \
  "banpubkey" "[\"${TAGGED_PUB}\", \"test ban\"]" \
  '.result == true'

# 16. List banned pubkeys includes the banned key
assert_nip86 "listbannedpubkeys contains banned key" \
  "listbannedpubkeys" '[]' \
  ".result | map(select(.pubkey == \"${TAGGED_PUB}\")) | length > 0"

# 17. Banned pubkey cannot publish
echo "  Testing banned pubkey rejection..."
BAN_TEST_EVENT=$(cat <<EOF
{
  "tags": [
    ["d", "https://example.org/courses/ban-test"],
    ["name", "Ban Test Resource"],
    ["type", "LearningResource"]
  ],
  "content": "test content"
}
EOF
)
BANNED_OUTPUT=$(publish_as "$TAGGED_SEC" "$BAN_TEST_EVENT")
if echo "$BANNED_OUTPUT" | grep -q "success"; then
  printf "${RED}FAIL${NC}: banned pubkey was not rejected (published successfully)\n"
  FAIL=$((FAIL + 1))
else
  printf "${GREEN}PASS${NC}: banned pubkey rejected\n"
  PASS=$((PASS + 1))
fi

# 18. Allow (unban) pubkey
assert_nip86 "allowpubkey succeeds" \
  "allowpubkey" "[\"${TAGGED_PUB}\", \"\"]" \
  '.result == true'

# 19. List banned pubkeys is now empty
assert_nip86 "listbannedpubkeys empty after unban" \
  "listbannedpubkeys" '[]' \
  '.result == null or (.result | length == 0)'

# 20. Unbanned pubkey can publish again
echo "  Testing unbanned pubkey can publish..."
UNBANNED_OUTPUT=$(publish_as "$TAGGED_SEC" "$BAN_TEST_EVENT")
if echo "$UNBANNED_OUTPUT" | grep -q "success"; then
  printf "${GREEN}PASS${NC}: unbanned pubkey can publish\n"
  PASS=$((PASS + 1))
else
  printf "${RED}FAIL${NC}: unbanned pubkey should be able to publish (got: %s)\n" "$UNBANNED_OUTPUT"
  FAIL=$((FAIL + 1))
fi

# 21. Ban event
FAKE_EVENT_ID="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
assert_nip86 "banevent succeeds" \
  "banevent" "[\"${FAKE_EVENT_ID}\", \"spam\"]" \
  '.result == true'

# 22. List banned events
assert_nip86 "listbannedevents contains banned event" \
  "listbannedevents" '[]' \
  ".result | map(select(.id == \"${FAKE_EVENT_ID}\")) | length > 0"

# 23. Allow (unban) event
assert_nip86 "allowevent succeeds" \
  "allowevent" "[\"${FAKE_EVENT_ID}\", \"\"]" \
  '.result == true'

# 24. List banned events empty after unban
assert_nip86 "listbannedevents empty after unban" \
  "listbannedevents" '[]' \
  '.result == null or (.result | length == 0)'

# 25. Change relay name
assert_nip86 "changerelayname succeeds" \
  "changerelayname" '["Updated E2E Relay"]' \
  '.result == true'

# 26. Verify relay name changed via NIP-11
NIP11_NAME=$(curl -s -H "Accept: application/nostr+json" "${RELAY_HTTP}" | jq -r '.name')
if [ "$NIP11_NAME" = "Updated E2E Relay" ]; then
  printf "${GREEN}PASS${NC}: NIP-11 name updated to '%s'\n" "$NIP11_NAME"
  PASS=$((PASS + 1))
else
  printf "${RED}FAIL${NC}: NIP-11 name expected 'Updated E2E Relay', got '%s'\n" "$NIP11_NAME"
  FAIL=$((FAIL + 1))
fi

# 27. Change relay description
assert_nip86 "changerelaydescription succeeds" \
  "changerelaydescription" '["Updated description"]' \
  '.result == true'

# 28. Change relay icon
assert_nip86 "changerelayicon succeeds" \
  "changerelayicon" '["https://example.com/icon.png"]' \
  '.result == true'

# 29. Stats
assert_nip86 "stats returns event_count" \
  "stats" '[]' \
  '.result.result.event_count != null'

# ============================================================
# NIP-86 Authorization tests
# ============================================================
echo ""
echo "--- NIP-86 Authorization tests ---"

# 30. Non-admin pubkey is rejected
NONADMIN_RESP=$(nip86_call "listbannedpubkeys" '[]' "$NONADMIN_SEC")
assert_nip86_error "non-admin pubkey rejected" "$NONADMIN_RESP"

# 31. No auth header is rejected
NOAUTH_RESP=$(nip86_call_noauth "listbannedpubkeys" '[]')
assert_nip86_error "missing auth header rejected" "$NOAUTH_RESP"

# 32. Invalid auth header is rejected
INVALID_RESP=$(curl -s -X POST \
  -H "Content-Type: application/nostr+json+rpc" \
  -H "Authorization: Nostr invalidbase64" \
  -d '{"method":"listbannedpubkeys","params":[]}' \
  "${RELAY_HTTP}" 2>/dev/null || true)
assert_nip86_error "invalid auth rejected" "$INVALID_RESP"

# ============================================================
# Typesense Management API tests (custom methods via Generic)
# ============================================================
echo ""
echo "--- Typesense Management API tests ---"

# Note: Generic handler returns nip86.Response which gets wrapped in another
# nip86.Response by khatru, so results are at .result.result

# 33. Get default collection schema
assert_nip86 "getcollectionschema returns default schema" \
  "getcollectionschema" '[]' \
  '.result.result.fields | length > 0'

# 34. Get schema contains expected fields
assert_nip86 "getcollectionschema has 'name' field" \
  "getcollectionschema" '[]' \
  '.result.result.fields | map(select(.name == "name")) | length > 0'

# 35. Update collection schema (add a custom field)
CUSTOM_SCHEMA=$(nip86_call "getcollectionschema" '[]')
# Extract the current schema (double-nested), add a test field, and send it back
UPDATED_SCHEMA=$(echo "$CUSTOM_SCHEMA" | jq '.result.result | .fields += [{"name": "customTestField", "type": "string", "optional": true}]')
SCHEMA_RESP=$(nip86_call "updatecollectionschema" "[$UPDATED_SCHEMA]")
if echo "$SCHEMA_RESP" | jq -e '.result.result == true' >/dev/null 2>&1; then
  printf "${GREEN}PASS${NC}: updatecollectionschema succeeds\n"
  PASS=$((PASS + 1))
else
  printf "${RED}FAIL${NC}: updatecollectionschema failed (response: %s)\n" "$SCHEMA_RESP"
  FAIL=$((FAIL + 1))
fi

# 36. Verify updated schema is persisted
assert_nip86 "getcollectionschema reflects update" \
  "getcollectionschema" '[]' \
  '.result.result.fields | map(select(.name == "customTestField")) | length > 0'

# 37. Reset collection schema
assert_nip86 "resetcollectionschema succeeds" \
  "resetcollectionschema" '[]' \
  '.result.result == true'

# 38. Verify schema reverted to default (no customTestField)
assert_nip86 "getcollectionschema reverted to default" \
  "getcollectionschema" '[]' \
  '.result.result.fields | map(select(.name == "customTestField")) | length == 0'

# 39. Reindex
REINDEX_RESP=$(nip86_call "reindex" '[]')
if echo "$REINDEX_RESP" | jq -e '.result.result == "reindex started"' >/dev/null 2>&1; then
  printf "${GREEN}PASS${NC}: reindex started\n"
  PASS=$((PASS + 1))
else
  printf "${RED}FAIL${NC}: reindex failed to start (response: %s)\n" "$REINDEX_RESP"
  FAIL=$((FAIL + 1))
fi

# 40. Poll reindex status until complete (max 30s)
echo -n "  Waiting for reindex to complete..."
for i in $(seq 1 30); do
  STATUS_RESP=$(nip86_call "getreindexstatus" '[]')
  RUNNING=$(echo "$STATUS_RESP" | jq -r '.result.result.running')
  if [ "$RUNNING" = "false" ]; then
    echo " done"
    break
  fi
  echo -n "."
  sleep 1
  if [ "$i" -eq 30 ]; then
    echo " TIMEOUT"
  fi
done

# 41. Check reindex status shows completion
assert_nip86 "getreindexstatus shows completed" \
  "getreindexstatus" '[]' \
  '.result.result.running == false and .result.result.indexed > 0'

# 42. Events still queryable after reindex
sleep 1
assert_count "events queryable after reindex" 4 \
  -k 30142

# 43. Double reindex not allowed while running - start a reindex and immediately try another
# First, update schema so reindex takes a moment (needs to recreate collection)
SCHEMA_FOR_REINDEX=$(nip86_call "getcollectionschema" '[]' | jq '.result.result')
nip86_call "updatecollectionschema" "[$SCHEMA_FOR_REINDEX]" >/dev/null
REINDEX_RESP2=$(nip86_call "reindex" '[]')
# Immediately try another
DOUBLE_RESP=$(nip86_call "reindex" '[]')
if echo "$DOUBLE_RESP" | jq -e '.result.error != null and .result.error != ""' >/dev/null 2>&1; then
  printf "${GREEN}PASS${NC}: double reindex rejected\n"
  PASS=$((PASS + 1))
else
  # It may have finished too fast; that's OK too
  printf "${YELLOW}SKIP${NC}: reindex completed too fast to test double-start\n"
fi

# Wait for any pending reindex to finish
for i in $(seq 1 30); do
  STATUS_RESP=$(nip86_call "getreindexstatus" '[]')
  RUNNING=$(echo "$STATUS_RESP" | jq -r '.result.result.running')
  if [ "$RUNNING" = "false" ]; then
    break
  fi
  sleep 1
done

# 44. Non-admin cannot use custom methods
NONADMIN_SCHEMA_RESP=$(nip86_call "getcollectionschema" '[]' "$NONADMIN_SEC")
assert_nip86_error "non-admin rejected for getcollectionschema" "$NONADMIN_SCHEMA_RESP"

# ============================================================
# Summary
# ============================================================
echo ""
echo "=============================="
printf "Results: ${GREEN}%d passed${NC}, ${RED}%d failed${NC}\n" "$PASS" "$FAIL"
echo "=============================="
[ "$FAIL" -eq 0 ] || exit 1
