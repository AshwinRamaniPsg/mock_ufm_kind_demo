#!/usr/bin/env bash
# Walk through the UFM REST contract against the mock.
#
#   ./demo.sh                              # against a local ./ufm-mock
#   UFM_URL=http://localhost:8080 ./demo.sh   # against a port-forwarded pod
#
# Every call below is one a real UFM Enterprise answers identically.
set -uo pipefail

UFM_URL="${UFM_URL:-http://127.0.0.1:9888}"
UFM_TOKEN="${UFM_TOKEN:-demo-token}"
AUTH="Authorization: Basic ${UFM_TOKEN}"
JSON="Content-Type: application/json"

pretty() { if command -v jq >/dev/null 2>&1; then jq .; else cat; echo; fi; }
step()   { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }
run()    { printf '\033[0;90m$ %s\033[0m\n' "$2"; eval "$2" | pretty; }

step "1. Who is answering? (GET /app/ufm_version)"
run - "curl -s -H '${AUTH}' ${UFM_URL}/ufmRestV3/app/ufm_version"

step "2. Subnet manager configuration (GET /app/smconf)"
run - "curl -s -H '${AUTH}' ${UFM_URL}/ufmRestV3/app/smconf"

step "3. Auth is enforced — no token means 401"
printf '\033[0;90m$ curl -s ... (no Authorization header)\033[0m\n'
curl -s -o /dev/null -w 'HTTP %{http_code}\n' "${UFM_URL}/ufmRestV3/app/ufm_version"

step "4. Discovered compute ports (GET /resources/ports)"
run - "curl -s -H '${AUTH}' ${UFM_URL}/ufmRestV3/resources/ports"

step "5. Partitions before any change — only the 0x7fff management pkey"
run - "curl -s -H '${AUTH}' '${UFM_URL}/ufmRestV3/resources/pkeys?guids_data=true&qos_conf=true'"

# Bind whatever the first two GUIDs happen to be.
GUIDS=$(curl -s -H "${AUTH}" "${UFM_URL}/ufmRestV3/resources/ports" \
  | grep -o '"guid":"[0-9a-f]*"' | cut -d'"' -f4 | head -2)
G1=$(echo "${GUIDS}" | sed -n 1p)
G2=$(echo "${GUIDS}" | sed -n 2p)

step "6. Tenant provisioning: bind ${G1} and ${G2} into pkey 0x1"
run - "curl -s -X POST -H '${AUTH}' -H '${JSON}' ${UFM_URL}/ufmRestV3/resources/pkeys \
  -d '{\"pkey\":\"0x1\",\"ip_over_ib\":true,\"membership\":\"full\",\"index0\":true,\"guids\":[\"${G1}\",\"${G2}\"]}'"

step "7. Read the new partition back — note the auto-generated name"
run - "curl -s -H '${AUTH}' '${UFM_URL}/ufmRestV3/resources/pkeys/0x1?guids_data=true&qos_conf=true'"

step "8. Rate-limit the tenant (PUT /resources/pkeys/qos_conf)"
run - "curl -s -X PUT -H '${AUTH}' -H '${JSON}' ${UFM_URL}/ufmRestV3/resources/pkeys/qos_conf \
  -d '{\"pkey\":\"0x1\",\"mtu_limit\":4,\"service_level\":3,\"rate_limit\":100}'"
run - "curl -s -H '${AUTH}' '${UFM_URL}/ufmRestV3/resources/pkeys/0x1?qos_conf=true'"

step "9. Binding an unknown GUID is rejected — the whole request fails"
printf '\033[0;90m$ curl ... guids:["0xdeadbeef"]\033[0m\n'
curl -s -w '\nHTTP %{http_code}\n' -X POST -H "${AUTH}" -H "${JSON}" \
  "${UFM_URL}/ufmRestV3/resources/pkeys" \
  -d '{"pkey":"0x2","ip_over_ib":false,"membership":"full","index0":false,"guids":["0xdeadbeef"]}'

step "10. Fabric change: a node's link goes down (POST /admin/inventory, generation 2)"
run - "curl -s -X POST -H '${JSON}' ${UFM_URL}/admin/inventory -d '{
  \"inventory_id\":\"demo-inventory\",\"epoch_id\":\"epoch-1\",\"generation\":2,
  \"machines\":[{\"mat_id\":\"mat-demo-a\",\"machine_id\":\"gpu-node-001\",
    \"infiniband_ports\":[{\"guid\":\"${G1}\",\"state\":\"down\"}]}]}'"

step "11. UFM now reports it down — and ${G2}, no longer advertised, is gone"
run - "curl -s -H '${AUTH}' ${UFM_URL}/ufmRestV3/resources/ports"
run - "curl -s -H '${AUTH}' '${UFM_URL}/ufmRestV3/resources/pkeys/0x1?guids_data=true'"

step "12. Replaying the same generation is ignored (idempotent reconciliation)"
run - "curl -s -X POST -H '${JSON}' ${UFM_URL}/admin/inventory -d '{
  \"inventory_id\":\"demo-inventory\",\"epoch_id\":\"epoch-1\",\"generation\":2,\"machines\":[]}'"

step "13. Chaos: make UFM return 503, the way a real outage looks to your client"
run - "curl -s -X POST -H '${JSON}' ${UFM_URL}/Injection/rules -d '{
  \"id\":\"ufm-outage\",
  \"selector\":{\"Path\":{\"method\":\"GET\",\"glob\":\"/ufmRestV3/app/ufm_version\"}},
  \"action\":{\"Status\":503}}'"
printf '\033[0;90m$ curl -s ... /app/ufm_version\033[0m\n'
curl -s -w '\nHTTP %{http_code}\n' -H "${AUTH}" "${UFM_URL}/ufmRestV3/app/ufm_version"
printf '\033[0;90m# other routes stay healthy:\033[0m '
curl -s -o /dev/null -w 'HTTP %{http_code}\n' -H "${AUTH}" "${UFM_URL}/ufmRestV3/resources/ports"

step "14. Clear the fault"
curl -s -X DELETE "${UFM_URL}/Injection/rules" >/dev/null
printf '# recovered: '
curl -s -o /dev/null -w 'HTTP %{http_code}\n' -H "${AUTH}" "${UFM_URL}/ufmRestV3/app/ufm_version"

step "15. Prometheus metrics (GET /metrics)"
curl -s "${UFM_URL}/metrics" | grep -v '^#'

step "16. Unbind — an emptied non-default partition disappears"
run - "curl -s -X POST -H '${AUTH}' -H '${JSON}' ${UFM_URL}/ufmRestV3/actions/remove_guids_from_pkey \
  -d '{\"pkey\":\"0x1\",\"guids\":[\"${G1}\"]}'"
run - "curl -s -H '${AUTH}' ${UFM_URL}/ufmRestV3/resources/pkeys"

printf '\n\033[1;32mDemo complete — back to a clean fabric with only 0x7fff.\033[0m\n'
