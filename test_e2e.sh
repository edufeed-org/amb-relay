#!/usr/bin/env bash
set -euo pipefail

TEST_PORT=13334
RELAY="ws://localhost:${TEST_PORT}"
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

# ============================================================
# Start relay
# ============================================================
echo "--- Starting relay ---"
export NAME="E2E Test Relay"
export PUBKEY=""
export DESCRIPTION="e2e test relay"
export ICON=""
export TS_APIKEY=xyz
export TS_HOST=http://localhost:8108
export TS_COLLECTION=amb_e2e_test
export PORT=$TEST_PORT

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
# Generate test keys
# ============================================================
SEC=$(nak key generate)
PUB=$(nak key public "$SEC")

# A second keypair used as a tagged pubkey in p-tags
TAGGED_SEC=$(nak key generate)
TAGGED_PUB=$(nak key public "$TAGGED_SEC")

echo "Test pubkey: $PUB"
echo "Tagged pubkey: $TAGGED_PUB"
echo ""

# ============================================================
# Helper functions
# ============================================================
publish() {
  local json="$1"
  local result
  result=$(echo "$json" | nak event -k 30142 --sec "$SEC" --auth "$RELAY" 2>&1) || true
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
# Summary
# ============================================================
echo ""
echo "=============================="
printf "Results: ${GREEN}%d passed${NC}, ${RED}%d failed${NC}\n" "$PASS" "$FAIL"
echo "=============================="
[ "$FAIL" -eq 0 ] || exit 1
