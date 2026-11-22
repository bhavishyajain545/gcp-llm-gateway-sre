# Demo transcript: demo

## 00:00  Architecture

This is an LLM gateway, built and run the way an SRE team would run it on Google Kubernetes Engine. A Go service sits in front of Gemini on Vertex AI and gives every team an OpenAI-compatible endpoint with its own API key, request and token quotas, an exact and a semantic cache in Memorystore, and automatic fallback to a cheaper model behind a circuit breaker. It runs on a GKE Standard cluster with an on-demand system pool and a Spot pool for the gateway, behind a global load balancer with Cloud Armor. Managed Prometheus feeds two SLOs and the autoscaler. Every resource is Terraform, and nothing in the path holds a key file.

## 00:40  First request

First, a real request through the load balancer. The models endpoint lists what this tenant may use, with the fallback and the list price per million tokens. Then a streamed chat completion with the cache bypassed, so this one goes to the model: the gateway translates it to Gemini's streaming API, relays the tokens as server-sent events, and reports time to first token. That first-token time is the latency a user feels, so it is the metric the SLO is built on.

## 01:06  Cache

The same question again is an exact cache hit: the answer comes back from Redis in a few milliseconds, with the x-cache header set to exact. A paraphrase of the question is a semantic hit: the gateway embeds the prompt, finds a stored prompt with cosine similarity above zero point nine five, and returns its answer without calling the model. Both caches are namespaced per tenant and per model, so one team can never be served another team's answer.

## 01:33  Quotas

Tenants are enforced at the gateway. team-support may only use flash-lite, so asking for the bigger model is a 403, not a silent downgrade. team-burst has a bucket of ten requests and a refill of thirty per minute: the first ten pass, then the gateway returns 429 with a Retry-After header. The bucket lives in Redis as a Lua script, so every replica shares one count. There is also a daily token budget per tenant, exposed in the budget headers.

## 02:03  Dashboard

The dashboard is twelve PromQL panels in Cloud Monitoring: first-token latency at p50 and p95, requests per second by outcome, tokens per second by model, cache hit ratio, estimated cost per hour per tenant, rate-limit rejections, upstream status codes, breaker state, and in-flight requests against replicas. Cost attribution per tenant comes for free from the same counters.

## 02:28  Autoscaling

Meanwhile a k6 load test has been running inside the cluster: a warm-cache scenario, a ramp of unique streaming prompts, and a low-quota tenant bursting. The autoscaler does not watch CPU, which stays low while pods wait on the model; it scales on in-flight requests per pod, a Prometheus gauge served through the custom metrics adapter. The deployment has grown from two replicas, and the cluster autoscaler added Spot nodes to hold them.

## 02:57  Fault drill

Now a fault drill. Through the admin endpoint I tell every replica to treat the primary model as failing for a minute. Requests keep succeeding, but the x-model-served header shows flash-lite: after a few failures the circuit breaker for the primary opens and traffic skips it entirely, then a single probe request closes it again when the minute is over. The breaker gauge and the fallback counter are visible in the metrics.

## 03:29  Cloud Armor

Cloud Armor sits at the edge, attached to the backend through a Gateway policy. A request carrying an SQL injection payload is rejected with 403 by the preconfigured WAF rule before it reaches a pod, and a burst of two hundred requests from one address is throttled to one hundred and twenty per minute with 429s. Tenant quotas protect the model budget; this protects the cluster.

## 03:53  SLOs

Two SLOs are defined in Terraform on the Prometheus metrics: ninety-nine point five percent of chat completions without a gateway or provider error, and ninety-five percent of streams delivering their first token within three seconds. That number came from the load test: cold first-token p95 was two point zero four seconds, so the objective was set where the system is, and two seconds is the stretch goal the cache buys. Quota rejections count as good; they are correct behaviour. Each SLO has a multi-window burn-rate alert, fourteen point four x over one hour or six x over six hours. The k6 thresholds encode the same targets, so a regression fails the load test before it burns budget.

## 04:36  Security

The security posture, verified: private nodes with Cloud NAT and a control plane limited to one network range; Workload Identity from the Kubernetes service account to a Google service account with exactly the roles it needs; API keys, the admin key and the Redis credentials read from Secret Manager, never from a Kubernetes secret; a default-deny network policy; and pods that are non-root, read-only and capability-free on a Pod Security restricted namespace.

## 05:05  Wrap-up

Everything shown is in the repository: the Go gateway with its tests, Terraform for the cluster, Memorystore, Cloud Armor, secrets, identities, SLOs and dashboard, the kustomize manifests, the k6 profile and a capacity and cost model. The whole platform costs a few hundred rupees a day while it runs, and the teardown script removes it. Thanks for watching.
