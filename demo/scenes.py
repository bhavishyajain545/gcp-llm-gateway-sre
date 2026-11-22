"""Demo video for Project 4: LLM Gateway SRE on GKE Standard.

Prerequisites:
  * platform deployed (`./deploy.sh`), gateway reachable through the load balancer, `./deploy.sh slo` done
  * kubeconfig + terraform state in WSL (gcloud/kubectl/terraform run there on Windows)
  * Chrome started with --remote-debugging-port=9222 and signed in to the Cloud console
  * DEMO_EXTRA_PATH with gcloud and ffmpeg

  python -u demo/scenes.py
"""

from __future__ import annotations

import asyncio
import json
import os
import subprocess
import time
from pathlib import Path

from recorder import Demo

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
PROJECT = "bj-llm-gateway"
ZONE = "us-central1-a"
NS = "llm-gateway"
CDP_URL = os.environ.get("DEMO_CDP_URL", "http://127.0.0.1:9222")

for p in os.environ.get("DEMO_EXTRA_PATH", "").split(os.pathsep):
    if p:
        os.environ["PATH"] = p + os.pathsep + os.environ["PATH"]
os.chdir(ROOT)


def wsl(cmd: str, timeout: int = 300) -> str:
    """Run a command inside WSL where gcloud/kubectl/terraform/curl are set up."""
    root = "/mnt/" + ROOT.drive[0].lower() + ROOT.as_posix()[2:]
    full = f"source ~/.gcp-env.sh; export USE_GKE_GCLOUD_AUTH_PLUGIN=True; cd {root}; {cmd}"
    try:
        r = subprocess.run(["wsl", "-e", "bash", "-c", full], capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=timeout)
        return (r.stdout + r.stderr).strip()
    except subprocess.TimeoutExpired:
        return "(timed out)"


def cdp_available() -> bool:
    import urllib.request
    try:
        with urllib.request.urlopen(CDP_URL + "/json/version", timeout=2):
            return True
    except Exception:
        return False


async def type_cmd(page, cmd: str, type_delay: int = 7, chunk: int = 4) -> None:
    """Type a command a few characters per CDP round-trip: one evaluate per character stalled
    behind the screencast frames (take 5: a 330-char command took 25 s to appear)."""
    await page.evaluate("(c) => window.__prompt(c)", "")
    for i in range(0, len(cmd), chunk):
        await page.evaluate("(c) => window.__type(c)", cmd[i:i + chunk])
        await asyncio.sleep(type_delay * chunk / 1000)


async def live_steps(page, steps, type_delay: int = 7, pause: float = 0.45, hold: float = 3.5) -> None:
    """Type (command, producer) pairs into the open terminal; producers run in a thread while the
    cursor blinks, so the recording never shows a stale frame."""
    for cmd, producer in steps:
        await type_cmd(page, cmd, type_delay)
        out = await asyncio.to_thread(producer) if callable(producer) else producer
        await asyncio.sleep(pause)
        await page.evaluate("(o) => window.__output(o)", out)
        await asyncio.sleep(pause)
    await asyncio.sleep(hold)


def trim(s: str, n: int = 1400) -> str:
    return s if len(s) <= n else s[:n] + "\n…"


NARRATION = {
    "arch": (
        "This is an LLM gateway, built and run the way an SRE team would run it on Google Kubernetes Engine. "
        "A Go service sits in front of Gemini on Vertex AI and gives every team an OpenAI-compatible endpoint with its own API key, "
        "request and token quotas, an exact and a semantic cache in Memorystore, and automatic fallback to a cheaper model behind a circuit breaker. "
        "It runs on a GKE Standard cluster with an on-demand system pool and a Spot pool for the gateway, behind a global load balancer with Cloud Armor. "
        "Managed Prometheus feeds two SLOs and the autoscaler. Every resource is Terraform, and nothing in the path holds a key file."
    ),
    "request": (
        "First, a real request through the load balancer. The models endpoint lists what this tenant may use, with the fallback and the list price per million tokens. "
        "Then a streamed chat completion with the cache bypassed, so this one goes to the model: the gateway translates it to Gemini's streaming API, relays the tokens as server-sent events, "
        "and reports time to first token. That first-token time is the latency a user feels, so it is the metric the SLO is built on."
    ),
    "cache": (
        "The same question again is an exact cache hit: the answer comes back from Redis in a few milliseconds, with the x-cache header set to exact. "
        "A paraphrase of the question is a semantic hit: the gateway embeds the prompt, finds a stored prompt with cosine similarity above zero point nine five, "
        "and returns its answer without calling the model. Both caches are namespaced per tenant and per model, so one team can never be served another team's answer."
    ),
    "quotas": (
        "Tenants are enforced at the gateway. team-support may only use flash-lite, so asking for the bigger model is a 403, not a silent downgrade. "
        "team-burst has a bucket of ten requests and a refill of thirty per minute: the first ten pass, then the gateway returns 429 with a Retry-After header. "
        "The bucket lives in Redis as a Lua script, so every replica shares one count. There is also a daily token budget per tenant, exposed in the budget headers."
    ),
    "hpa": (
        "Meanwhile a k6 load test has been running inside the cluster: a warm-cache scenario, a ramp of unique streaming prompts, and a low-quota tenant bursting. "
        "The autoscaler does not watch CPU, which stays low while pods wait on the model; it scales on in-flight requests per pod, a Prometheus gauge served through the custom metrics adapter. "
        "The deployment has grown from two replicas, and the cluster autoscaler added Spot nodes to hold them."
    ),
    "dashboard": (
        "The dashboard is twelve PromQL panels in Cloud Monitoring: first-token latency at p50 and p95, requests per second by outcome, tokens per second by model, "
        "cache hit ratio, estimated cost per hour per tenant, rate-limit rejections, upstream status codes, breaker state, and in-flight requests against replicas. "
        "Cost attribution per tenant comes for free from the same counters."
    ),
    "chaos": (
        "Now a fault drill. Through the admin endpoint I tell every replica to treat the primary model as failing for a minute. "
        "Requests keep succeeding, but the x-model-served header shows flash-lite: after a few failures the circuit breaker for the primary opens and traffic skips it entirely, "
        "then a single probe request closes it again when the minute is over. The breaker gauge and the fallback counter are visible in the metrics."
    ),
    "armor": (
        "Cloud Armor sits at the edge, attached to the backend through a Gateway policy. A request carrying an SQL injection payload is rejected with 403 by the preconfigured WAF rule before it reaches a pod, "
        "and a burst of two hundred requests from one address is throttled to one hundred and twenty per minute with 429s. Tenant quotas protect the model budget; this protects the cluster."
    ),
    "slo": (
        "Two SLOs are defined in Terraform on the Prometheus metrics: ninety-nine point five percent of chat completions without a gateway or provider error, "
        "and ninety-five percent of streams delivering their first token within three seconds. That number came from the load test: cold first-token p95 was two point zero four seconds, so the objective was set where the system is, and two seconds is the stretch goal the cache buys. Quota rejections count as good; they are correct behaviour. "
        "Each SLO has a multi-window burn-rate alert, fourteen point four x over one hour or six x over six hours. "
        "The k6 thresholds encode the same targets, so a regression fails the load test before it burns budget."
    ),
    "security": (
        "The security posture, verified: private nodes with Cloud NAT and a control plane limited to one network range; Workload Identity from the Kubernetes service account to a Google service account with exactly the roles it needs; "
        "API keys, the admin key and the Redis credentials read from Secret Manager, never from a Kubernetes secret; a default-deny network policy; and pods that are non-root, read-only and capability-free on a Pod Security restricted namespace."
    ),
    "wrap": (
        "Everything shown is in the repository: the Go gateway with its tests, Terraform for the cluster, Memorystore, Cloud Armor, secrets, identities, SLOs and dashboard, "
        "the kustomize manifests, the k6 profile and a capacity and cost model. The whole platform costs a few hundred rupees a day while it runs, and the teardown script removes it. Thanks for watching."
    ),
}


async def main() -> None:
    use_cdp = cdp_available()
    demo = Demo(out_dir=HERE, cdp_url=CDP_URL if use_cdp else None, max_silence=5.0, tail=4.0)
    await demo.prepare(*NARRATION.values())

    # ---- real data for the terminal scenes (collected before recording) ----
    t0 = time.time()
    ip = wsl(f"kubectl -n {NS} get gateway external -o jsonpath='{{.status.addresses[0].value}}'")
    api = f"http://{ip}"
    keys = json.loads(wsl("terraform -chdir=terraform output -json tenant_keys"))
    admin = wsl("terraform -chdir=terraform output -raw admin_key")
    k_plat, k_sup, k_burst = keys["team-platform"], keys["team-support"], keys["team-burst"]
    dash_url = wsl("terraform -chdir=terraform output -raw dashboard_url")
    slo_url = wsl("terraform -chdir=terraform output -raw slo_url")
    cluster = wsl(f"gcloud container clusters describe llm-gateway --zone {ZONE} --project {PROJECT} --format='value(privateClusterConfig.enablePrivateNodes,masterAuthorizedNetworksConfig.cidrBlocks[0].cidrBlock,workloadIdentityConfig.workloadPool,networkConfig.datapathProvider)'")
    ksa = wsl(f"kubectl -n {NS} get sa llm-gateway -o jsonpath='{{.metadata.annotations.iam\\.gke\\.io/gcp-service-account}}'")
    gsa_roles = wsl(f"gcloud projects get-iam-policy {PROJECT} --flatten='bindings[].members' --filter='bindings.members:llm-gateway@' --format='value(bindings.role)'")
    sm_keys = wsl(f"gcloud iam service-accounts keys list --iam-account llm-gateway@{PROJECT}.iam.gserviceaccount.com --managed-by=user 2>&1")
    secrets = wsl(f"gcloud secrets list --project {PROJECT} --format='value(name)'")
    env_secrets = wsl(f"kubectl -n {NS} get deploy llm-gateway -o jsonpath='{{range .spec.template.spec.containers[0].env[*]}}{{.name}}={{.value}}{{\"\\n\"}}{{end}}' | grep SECRET")
    k8s_secrets = wsl(f"kubectl -n {NS} get secrets --field-selector type=Opaque -o name 2>&1 | grep -v k6-keys || echo 'No resources found in llm-gateway namespace.'")
    sec_ctx = wsl(f"kubectl -n {NS} get deploy llm-gateway -o jsonpath='{{.spec.template.spec.securityContext}}{{\"\\n\"}}{{.spec.template.spec.containers[0].securityContext}}'")
    netpol = wsl(f"kubectl -n {NS} get networkpolicy")
    ns_labels = wsl(f"kubectl get ns {NS} -o jsonpath='{{.metadata.labels}}'")
    armor_rules = wsl(f"gcloud compute security-policies describe llm-gateway-edge --project {PROJECT} --format='value(rules[].description)' | tr ';' '\\n'")
    tf_count = str(len(wsl("terraform -chdir=terraform state list").splitlines()))
    tf_output = wsl("terraform -chdir=terraform output -no-color | grep -v -E 'tenant_keys|admin_key'")
    print(f"pre-computed in {time.time() - t0:.0f}s; api={api}")

    def curl(args: str, timeout: int = 90) -> str:
        return wsl(f"curl -s --max-time 60 {args}", timeout=timeout)

    page = await demo.start()
    dash_tab = slo_tab = None
    if use_cdp:
        dash_tab = await demo.new_tab(dash_url)
        slo_tab = await demo.new_tab(slo_url)
        await demo.capture(page)

    # ---- 1 architecture ----
    async def s1():
        await page.goto((ROOT / "docs" / "architecture.html").as_uri())
        await page.evaluate("() => { document.body.style.zoom = '1.33'; document.body.style.margin = '0'; }")
    await demo.scene("Architecture", NARRATION["arch"], s1)

    # k6 runs in the background from here on (≈2.5 min) so the HPA scene has something to show.
    k6_log: dict = {}

    def run_k6():
        k6_log["out"] = wsl("DURATION=150s PEAK_VUS=24 loadtest/run.sh 2>&1 | tail -60", timeout=420)

    k6_task = asyncio.ensure_future(asyncio.to_thread(run_k6))

    # ---- 2 first request ----
    q = "What is an error budget? Answer in two sentences."
    body_stream = json.dumps({"model": "gemini-3.8-flash", "stream": True, "messages": [{"role": "user", "content": q}]})
    body_plain = json.dumps({"model": "gemini-3.8-flash", "messages": [{"role": "user", "content": q}]})
    tokens_filter = ("tee /tmp/sse.txt >/dev/null; grep '^data: {' /tmp/sse.txt | sed 's/^data: //' | jq -rj '.choices[0].delta.content // empty' | fold -s -w 150; "
                     "echo; echo '[DONE]'; tail -1 /tmp/sse.txt")

    async def s2():
        await demo.terminal("llm-gateway through the global load balancer", [])
        await live_steps(page, [
            (f"export GW={api} KEY=$(terraform output -raw tenant_key_team_platform)", ""),
            ("curl -s $GW/v1/models -H \"Authorization: Bearer $KEY\" | jq -c '.data[]'",
             lambda: curl(f"{api}/v1/models -H 'Authorization: Bearer {k_plat}' | jq -c '.data[]'")),
            ("curl -sN $GW/v1/chat/completions -H \"Authorization: Bearer $KEY\" -H 'Cache-Control: no-cache' -d '{\"model\":\"gemini-3.8-flash\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"" + q + "\"}]}' -w '\\nTTFT %{time_starttransfer}s  total %{time_total}s' | jq -rj '.choices[0].delta.content // empty'",
             lambda: trim(curl(f"-N {api}/v1/chat/completions -H 'Authorization: Bearer {k_plat}' -H 'content-type: application/json' -H 'Cache-Control: no-cache' -d '{body_stream}' -w '\\nTTFT %{{time_starttransfer}}s  total %{{time_total}}s' | {tokens_filter}"), 1800)),
        ], type_delay=5)
    await demo.scene("First request", NARRATION["request"], s2, hold=1.0)

    # ---- 3 cache ----
    q2 = "In two sentences, what is an error budget?"
    body_para = json.dumps({"model": "gemini-3.8-flash", "messages": [{"role": "user", "content": q2}]})

    async def s3():
        await demo.terminal("Exact and semantic cache (Memorystore)", [])
        hdr = "-D - -o /dev/null -w 'time_total %{time_total}s'"
        await live_steps(page, [
            ("curl -s $GW/v1/chat/completions -D - -o /dev/null -H \"Authorization: Bearer $KEY\" -d '…same question…' -w 'time_total %{time_total}s' | grep -iE 'x-cache|x-model|time_total'",
             lambda: curl(f"{api}/v1/chat/completions {hdr} -H 'Authorization: Bearer {k_plat}' -H 'content-type: application/json' -d '{body_plain}' | grep -iE 'x-cache|x-model|time_total'")),
            ("curl -s $GW/v1/chat/completions -D - -o /dev/null -H \"Authorization: Bearer $KEY\" -d '{\"messages\":[{\"role\":\"user\",\"content\":\"" + q2 + "\"}]}' -w 'time_total %{time_total}s' | grep -iE 'x-cache|x-model|time_total'",
             lambda: curl(f"{api}/v1/chat/completions {hdr} -H 'Authorization: Bearer {k_plat}' -H 'content-type: application/json' -d '{body_para}' | grep -iE 'x-cache|x-model|time_total'")),
            ("curl -s $GW/v1/chat/completions -H \"Authorization: Bearer $KEY\" -d '…paraphrase…' | jq -r '.choices[0].message.content'",
             lambda: curl(f"{api}/v1/chat/completions -H 'Authorization: Bearer {k_plat}' -H 'content-type: application/json' -d '{body_para}' | jq -r '.choices[0].message.content' | fold -s -w 150")),
        ], type_delay=5)
    await demo.scene("Cache", NARRATION["cache"], s3, hold=1.0)

    # ---- 4 tenants and quotas ----
    async def s4():
        await demo.terminal("Tenant keys, allow-lists and token-bucket quotas", [])
        burst_body = json.dumps({"model": "gemini-3.5-flash-lite", "max_tokens": 20, "messages": [{"role": "user", "content": "Say ok."}]})
        await live_steps(page, [
            ("curl -s $GW/v1/chat/completions -H \"Authorization: Bearer $KEY_SUPPORT\" -d '{\"model\":\"gemini-3.8-flash\", …}' -w ' HTTP %{http_code}'",
             lambda: curl(f"{api}/v1/chat/completions -H 'Authorization: Bearer {k_sup}' -H 'content-type: application/json' -d '{body_plain}' -w ' HTTP %{{http_code}}'")),
            ("for i in $(seq 1 14); do curl -s -o /dev/null -w '%{http_code} ' $GW/v1/chat/completions -H \"Authorization: Bearer $KEY_BURST\" -d \"{…prompt $i…}\"; done",
             lambda: wsl(f"for i in $(seq 1 14); do curl -s --max-time 30 -o /dev/null -w '%{{http_code}} ' {api}/v1/chat/completions -H 'Authorization: Bearer {k_burst}' -H 'content-type: application/json' -d '{{\"model\":\"gemini-3.5-flash-lite\",\"max_tokens\":20,\"messages\":[{{\"role\":\"user\",\"content\":\"Say ok number '$i'.\"}}]}}'; done", timeout=400)),
            ("curl -s $GW/v1/chat/completions -D - -H \"Authorization: Bearer $KEY_BURST\" -d '…' | grep -iE 'HTTP/|retry-after|x-ratelimit|x-budget|\"message\"'",
             lambda: curl(f"{api}/v1/chat/completions -D - -H 'Authorization: Bearer {k_burst}' -H 'content-type: application/json' -d '{burst_body}' | grep -iE 'HTTP/|retry-after|x-ratelimit|x-budget|\"message\"'")),
        ], type_delay=5)
    await demo.scene("Quotas", NARRATION["quotas"], s4, hold=1.0)

    # ---- 6 dashboard ----
    if use_cdp:
        async def s6():
            await demo.capture(dash_tab)
            try:
                await dash_tab.wait_for_selector("text=TTFT", timeout=8000)
            except Exception:
                await dash_tab.reload(wait_until="commit")
                try:
                    await dash_tab.wait_for_selector("text=TTFT", timeout=60000)
                except Exception:
                    pass
            await asyncio.sleep(8)
            await dash_tab.mouse.move(1100, 650)
            await dash_tab.mouse.wheel(0, 450)
            await asyncio.sleep(6)
            await dash_tab.mouse.wheel(0, 450)
        await demo.scene("Dashboard", NARRATION["dashboard"], s6, hold=1.5)
        await demo.capture(page)

    # ---- 5 HPA + Spot scale-out ----
    async def s5():
        await demo.terminal("Autoscaling on in-flight requests (custom metric) + Spot nodes", [])
        await live_steps(page, [
            (f"kubectl -n {NS} get job k6; kubectl -n {NS} logs job/k6 --tail=3", lambda: wsl(f"kubectl -n {NS} get job k6; kubectl -n {NS} logs job/k6 --tail=3 2>/dev/null | cut -c1-140")),
            (f"kubectl -n {NS} get hpa llm-gateway", lambda: wsl(f"kubectl -n {NS} get hpa llm-gateway")),
            ("kubectl get --raw \"/apis/custom.metrics.k8s.io/v1beta2/namespaces/llm-gateway/pods/*/prometheus.googleapis.com|llm_inflight_requests|gauge\" | jq -c '.items[] | {pod: .describedObject.name, value: .value}'",
             lambda: wsl("kubectl get --raw \"/apis/custom.metrics.k8s.io/v1beta2/namespaces/llm-gateway/pods/*/prometheus.googleapis.com|llm_inflight_requests|gauge\" | jq -c '.items[] | {pod: .describedObject.name, value: .value}'")),
            (f"kubectl -n {NS} get pods -l app=llm-gateway -o wide | cut -c1-120", lambda: wsl(f"kubectl -n {NS} get pods -l app=llm-gateway -o wide | cut -c1-120")),
            ("kubectl get nodes -L pool,cloud.google.com/gke-spot", lambda: wsl("kubectl get nodes -L pool,cloud.google.com/gke-spot | cut -c1-120")),
        ], type_delay=5)
    await demo.scene("Autoscaling", NARRATION["hpa"], s5, hold=1.0)

    # ---- 7 chaos: fallback + breaker ----
    async def s7():
        await demo.terminal("Fault drill: primary model failing → fallback + circuit breaker", [])
        chaos = json.dumps({"model": "gemini-3.8-flash", "mode": "error", "seconds": 75})
        uniq = lambda i: json.dumps({"model": "gemini-3.8-flash", "max_tokens": 40, "messages": [{"role": "user", "content": f"Drill {i} {int(time.time())}: name one SRE principle."}]})
        await live_steps(page, [
            ("curl -s -X POST $GW/admin/chaos -H \"x-admin-key: $ADMIN\" -d '{\"model\":\"gemini-3.8-flash\",\"mode\":\"error\",\"seconds\":75}'",
             lambda: curl(f"-X POST {api}/admin/chaos -H 'x-admin-key: {admin}' -H 'content-type: application/json' -d '{chaos}'")),
            ("for i in $(seq 1 10); do curl -s $GW/v1/chat/completions -D - -o /dev/null -H \"Authorization: Bearer $KEY\" -d \"{…unique prompt $i…}\" | grep -iE 'x-model-served'; done",
             lambda: "\n".join(curl(f"{api}/v1/chat/completions -D - -o /dev/null -H 'Authorization: Bearer {k_plat}' -H 'content-type: application/json' -d '{uniq(i)}' | grep -iE 'x-model-served' | tr -d '\\r'") for i in range(1, 11))),
            (f"kubectl -n {NS} port-forward deploy/llm-gateway 18081:8080 & curl -s localhost:18081/metrics | grep -E '^llm_(breaker_open|fallbacks_total|faults_applied_total)'",
             lambda: wsl(f"(timeout 45 kubectl -n {NS} port-forward deploy/llm-gateway 18081:8080 >/dev/null 2>&1 &) ; for i in $(seq 1 60); do curl -sf localhost:18081/metrics >/tmp/m.txt 2>/dev/null && break; sleep 0.5; done; grep -E '^llm_(breaker_open|fallbacks_total|faults_applied_total)' /tmp/m.txt", timeout=60)),
            ("curl -s -X POST $GW/admin/chaos -H \"x-admin-key: $ADMIN\" -d '{\"model\":\"gemini-3.8-flash\",\"mode\":\"clear\"}'",
             lambda: curl(f"-X POST {api}/admin/chaos -H 'x-admin-key: {admin}' -H 'content-type: application/json' -d '{{\"model\":\"gemini-3.8-flash\",\"mode\":\"clear\"}}'")),
        ], type_delay=5)
    await demo.scene("Fault drill", NARRATION["chaos"], s7, hold=1.0)

    # ---- 8 Cloud Armor ----
    async def s8():
        await demo.terminal("Cloud Armor at the edge: WAF + per-IP throttle", [])
        await live_steps(page, [
            ("gcloud compute security-policies describe llm-gateway-edge --format='value(rules[].description)'", armor_rules),
            ("curl -s -o /dev/null -w 'HTTP %{http_code}\\n' \"$GW/healthz?q=%27%20or%201%3D1%20--%20\"",
             lambda: curl(f"-o /dev/null -w 'HTTP %{{http_code}}\\n' '{api}/healthz?q=%27%20or%201%3D1%20--%20'")),
            ("seq 1 200 | xargs -P 20 -I{} curl -s -o /dev/null -w '%{http_code}\\n' $GW/healthz | sort | uniq -c",
             lambda: wsl(f"seq 1 200 | xargs -P 20 -I{{}} curl -s --max-time 20 -o /dev/null -w '%{{http_code}}\\n' {api}/healthz | sort | uniq -c", timeout=180)),
        ], type_delay=5)
    await demo.scene("Cloud Armor", NARRATION["armor"], s8, hold=1.0)

    # ---- 9 SLOs + k6 thresholds ----
    if not k6_task.done():
        try:
            await asyncio.wait_for(k6_task, timeout=200)
        except Exception:
            pass
    k6_out = k6_log.get("out", "")
    k6_summary = "\n".join(l for l in k6_out.splitlines() if any(k in l for k in ("✓", "✗", "ttft_", "cache_hits", "rate_limited", "gateway_errors", "http_req_failed", "http_reqs", "completion_tokens", "checks")))

    if use_cdp:
        async def s9():
            await demo.capture(slo_tab)
            try:
                await slo_tab.wait_for_selector("text=first token", timeout=8000)
            except Exception:
                await slo_tab.reload(wait_until="commit")
                try:
                    await slo_tab.wait_for_selector("text=first token", timeout=90000)
                except Exception:
                    pass
            await asyncio.sleep(12)
            await demo.capture(page)
            await demo.terminal("k6 thresholds = the SLOs", [])
            await live_steps(page, [(f"kubectl -n {NS} logs job/k6 | grep -E 'ttft|cache_hits|rate_limited|gateway_errors|http_req_failed|http_reqs|✓|✗'", trim(k6_summary, 1600))], type_delay=5)
        await demo.scene("SLOs", NARRATION["slo"], s9, hold=1.0)
    else:
        async def s9b():
            await demo.terminal("SLOs + k6 thresholds", [])
            await live_steps(page, [("terraform state list | grep monitoring_slo", wsl("terraform -chdir=terraform state list | grep monitoring_slo")),
                                    (f"kubectl -n {NS} logs job/k6 | tail -30", trim(k6_summary, 1600))])
        await demo.scene("SLOs", NARRATION["slo"], s9b)

    # ---- 10 security ----
    async def s10():
        await demo.terminal("Security posture, verified", [])
        await live_steps(page, [
            (f"gcloud container clusters describe llm-gateway --zone {ZONE} --format='value(privateClusterConfig.enablePrivateNodes,masterAuthorizedNetworksConfig.cidrBlocks[0].cidrBlock,workloadIdentityConfig.workloadPool,networkConfig.datapathProvider)'", cluster),
            (f"kubectl -n {NS} get sa llm-gateway -o jsonpath='{{.metadata.annotations}}'", f'{{"iam.gke.io/gcp-service-account":"{ksa}"}}'),
            (f"gcloud projects get-iam-policy {PROJECT} --filter='bindings.members:llm-gateway@' --format='value(bindings.role)'", gsa_roles),
            (f"gcloud iam service-accounts keys list --iam-account {ksa} --managed-by=user", sm_keys or "Listed 0 items."),
            (f"gcloud secrets list --format='value(name)'; kubectl -n {NS} get deploy llm-gateway -o jsonpath='…env…' | grep SECRET", secrets + "\n" + env_secrets),
            (f"kubectl -n {NS} get secrets --field-selector type=Opaque", k8s_secrets),
            (f"kubectl get ns {NS} -o jsonpath='{{.metadata.labels}}'; kubectl -n {NS} get networkpolicy", ns_labels + "\n" + netpol),
            (f"kubectl -n {NS} get deploy llm-gateway -o jsonpath='{{.spec.template.spec.securityContext}}{{.spec.template.spec.containers[0].securityContext}}'", sec_ctx),
        ], type_delay=4, pause=0.4)
    await demo.scene("Security", NARRATION["security"], s10, hold=1.0)

    # ---- 11 wrap ----
    async def s11():
        await demo.terminal("Infrastructure as code + cleanup", [])
        await live_steps(page, [
            ("terraform -chdir=terraform state list | wc -l", tf_count),
            ("terraform -chdir=terraform output", tf_output),
            ("# when done: ./teardown.sh", "k8s objects → terraform destroy → load-balancer leftovers → destroy again → billing audit"),
        ], type_delay=6)
    await demo.scene("Wrap-up", NARRATION["wrap"], s11, hold=1.5)

    await demo.finish("demo")


if __name__ == "__main__":
    asyncio.run(main())
