#!/usr/bin/env bash
# Alarms and the EFS (Events Fluent Streaming) pipeline, end to end.
#
#   ./demo-efs.sh                                     # against the pods
#   UFM_URL=http://127.0.0.1:19888 ./demo-efs.sh      # against local processes
#
# The pipeline being demonstrated:
#
#   mock UFM  --syslog/UDP-->  EFS plugin  --Fluent Forward-->  collector
#
set -uo pipefail

UFM_URL="${UFM_URL:-http://127.0.0.1:9888}"
UFM_TOKEN="${UFM_TOKEN:-demo-token}"
EFS_URL="${EFS_URL:-http://127.0.0.1:8989}"
# Where the mock should send syslog. Inside Kubernetes this is the EFS Service.
SYSLOG_DEST="${SYSLOG_DEST:-ufm-efs.ufm-demo.svc.cluster.local:5140}"

AUTH="Authorization: Basic ${UFM_TOKEN}"
JSON="Content-Type: application/json"

pretty() { if command -v jq >/dev/null 2>&1; then jq .; else cat; echo; fi; }
step()   { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }
note()   { printf '\033[0;90m%s\033[0m\n' "$*"; }
run()    { printf '\033[0;90m$ %s\033[0m\n' "$1"; eval "$1" | pretty; }

step "1. The EFS plugin's current configuration (GET /plugin/efs/conf)"
note "Same schema as conf/ufm_syslog_streaming_plugin.cfg upstream."
run "curl -s ${EFS_URL}/plugin/efs/conf"

step "2. Turn on streaming (PUT /plugin/efs/conf)"
note "A partial PUT merges — everything not mentioned keeps its value."
run "curl -s -X PUT -H '${JSON}' ${EFS_URL}/plugin/efs/conf -d '{
  \"streaming\": {\"enabled\": true},
  \"fluent-bit-endpoint\": {\"enabled\": true}}'"

step "3. Point UFM's syslog at the EFS plugin (PUT /app/syslog)"
note "level=WARNING means Info and Debug events will not be streamed."
run "curl -s -X PUT -H '${AUTH}' -H '${JSON}' ${UFM_URL}/ufmRestV3/app/syslog -d '{
  \"active\": true,
  \"destination\": \"${SYSLOG_DEST}\",
  \"level\": \"WARNING\",
  \"ufm_log\": true,
  \"events_log\": true}'"

step "4. Raise a Critical event — a link goes down"
run "curl -s -X POST -H '${AUTH}' -H '${JSON}' ${UFM_URL}/ufmRestV3/app/events/external_event -d '{
  \"event_id\": 331,
  \"name\": \"Link Down\",
  \"severity\": \"Critical\",
  \"object_name\": \"gpu-node-002\",
  \"otype\": \"IBPort\",
  \"category\": \"Hardware\",
  \"description\": \"Port b8cef60300000004 went down\"}'"

step "5. Raise a Warning on another device"
run "curl -s -X POST -H '${AUTH}' -H '${JSON}' ${UFM_URL}/ufmRestV3/app/events/external_event -d '{
  \"event_id\": 110,
  \"name\": \"High BER\",
  \"severity\": \"Warning\",
  \"object_name\": \"gpu-node-001\",
  \"otype\": \"IBPort\",
  \"category\": \"Hardware\",
  \"description\": \"Symbol error rate above threshold\"}'"

step "6. Raise an Info event — below the WARNING filter, must NOT stream"
run "curl -s -X POST -H '${AUTH}' -H '${JSON}' ${UFM_URL}/ufmRestV3/app/events/external_event -d '{
  \"event_id\": 67,
  \"name\": \"MCast Group Deleted\",
  \"severity\": \"Info\",
  \"object_name\": \"default\",
  \"description\": \"routine churn, should be filtered out\"}'"

sleep 1

step "7. Active alarms (GET /app/alarms)"
note "Only Warning and above raise a standing alarm. Info did not."
run "curl -s -H '${AUTH}' ${UFM_URL}/ufmRestV3/app/alarms"

step "8. The same fault again — bumps event_count, does not stack a new alarm"
run "curl -s -X POST -H '${AUTH}' -H '${JSON}' ${UFM_URL}/ufmRestV3/app/events/external_event -d '{
  \"event_id\": 331,
  \"name\": \"Link Down\",
  \"severity\": \"Critical\",
  \"object_name\": \"gpu-node-002\",
  \"otype\": \"IBPort\",
  \"description\": \"flapped again\"}' > /dev/null; \
  curl -s -H '${AUTH}' ${UFM_URL}/ufmRestV3/app/alarms"

step "9. What EFS received and forwarded (GET /plugin/efs/stats)"
note "received should be 3: two Critical plus one Warning. The Info never left UFM."
run "curl -s ${EFS_URL}/plugin/efs/stats"

step "10. Filter alarms by device (GET /app/alarms?device_id=)"
run "curl -s -H '${AUTH}' '${UFM_URL}/ufmRestV3/app/alarms?device_id=gpu-node-001'"

step "11. Clear one device's alarms (DELETE /app/alarms?device_id=)"
run "curl -s -X DELETE -H '${AUTH}' '${UFM_URL}/ufmRestV3/app/alarms?device_id=gpu-node-001'"
run "curl -s -H '${AUTH}' ${UFM_URL}/ufmRestV3/app/alarms"

step "12. Clear the rest"
run "curl -s -X DELETE -H '${AUTH}' ${UFM_URL}/ufmRestV3/app/alarms"
run "curl -s -H '${AUTH}' ${UFM_URL}/ufmRestV3/app/alarms"

step "13. Events remain as an audit trail"
note "Clearing alarms never destroys history."
run "curl -s -H '${AUTH}' ${UFM_URL}/ufmRestV3/app/events"

printf '\n\033[1;32mEFS demo complete.\033[0m\n'
printf 'Watch the collector receive these live:\n'
printf '  kubectl -n ufm-demo logs -f deploy/ufm-collector\n'
printf 'Or locally, whichever terminal is running: ./ufm-mock sink\n\n'
