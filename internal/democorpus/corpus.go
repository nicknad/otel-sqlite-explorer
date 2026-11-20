// Package democorpus holds the demo log corpus shared by the two demo-data
// adapters: cmd/mockdb (fast, Docker-less synthetic writes) and cmd/sinkdemo
// (the true ingestion path through the published sink container). One corpus,
// two adapters — the services, body templates, attribute specs, severity
// weights and upstream names live here so the two generators cannot drift.
package democorpus

// Attr is one attribute spec emitted on generated log events. Kind is one
// of "str", "int", "float", "bool", "http_status", "http_method".
type Attr struct {
	Key  string
	Kind string
}

// Service describes a mock emitting service: its host, body templates
// (filled with %s / %d placeholders), the attributes it emits, and the pool
// of string values used for "str" kind attributes.
type Service struct {
	Name   string
	Host   string
	Bodies []string
	Attrs  []Attr
	Strs   []string
}

// SeverityWeights biases the demo traffic towards a realistic log
// distribution (INFO commonest, FATAL rare). Text and number pairs come
// from severity.All so they stay in sync with the UI's severity filter.
var SeverityWeights = map[string]int{"DEBUG": 8, "INFO": 62, "WARN": 18, "ERROR": 10, "FATAL": 2}

// Upstreams are the service names substituted into %s body placeholders.
var Upstreams = []string{"auth-svc", "checkout-svc", "payments-svc", "inventory-svc"}

// Services lists the emitting services in priority order (earlier entries
// generate more traffic; see the weighted pickers in each adapter).
var Services = []Service{
	{
		Name: "api-gateway",
		Host: "gw-prod-01",
		Bodies: []string{
			"connection timeout to upstream %s",
			"request completed with status %d",
			"rate limit exceeded for client %d",
			"upstream 502 from %s",
			"retrying upstream call attempt %d",
			"invalid request rejected",
		},
		Attrs: []Attr{
			{"http.method", "http_method"},
			{"http.route", "str"},
			{"http.status_code", "http_status"},
			{"http.user_agent", "str"},
			{"client.ip", "str"},
			{"request.id", "int"},
		},
		Strs: []string{"/api/v1/orders", "/api/v1/auth/login", "/api/v1/payments", "/healthz", "/api/v1/inventory",
			"curl/8.0", "Go-http-client/2.0", "Mozilla/5.0", "python-requests/2.31",
			"10.0.4.21", "10.0.4.22", "172.16.8.4", "172.16.8.9"},
	},
	{
		Name: "auth-svc",
		Host: "auth-prod-02",
		Bodies: []string{
			"token validation failed for user %d",
			"login success for user %d",
			"password reset requested for user %d",
			"invalid credentials attempt for user %d",
			"refresh token rotation",
			"MFA challenge issued",
		},
		Attrs: []Attr{
			{"auth.user_id", "int"},
			{"auth.tenant_id", "int"},
			{"auth.mfa", "bool"},
			{"auth.provider", "str"},
			{"http.status_code", "http_status"},
		},
		Strs: []string{"oidc", "okta", "github", "saml", "email", "passwordless"},
	},
	{
		Name: "checkout-svc",
		Host: "checkout-prod-01",
		Bodies: []string{
			"order placed",
			"payment processing for order %d",
			"inventory reservation failed for sku %s",
			"cart updated for user %d",
			"order %d finalized",
			"coupon applied",
		},
		Attrs: []Attr{
			{"order.id", "int"},
			{"order.total", "float"},
			{"order.currency", "str"},
			{"cart.items", "int"},
			{"user.id", "int"},
		},
		Strs: []string{"USD", "EUR", "GBP", "CAD"},
	},
	{
		Name: "payments-svc",
		Host: "pay-prod-03",
		Bodies: []string{
			"charge succeeded for order %d",
			"charge declined for order %d",
			"refund issued for charge %s",
			"webhook signature verification failed",
			"payout scheduled",
			"gateway timeout for charge %s",
		},
		Attrs: []Attr{
			{"charge.id", "str"},
			{"order.id", "int"},
			{"payment.amount", "float"},
			{"payment.method", "str"},
			{"payment.status_code", "http_status"},
			{"card.brand", "str"},
		},
		Strs: []string{"ch_3NqWx9", "ch_3NrA1y", "ch_3NoC3q", "visa", "mastercard", "apple_pay", "amex"},
	},
	{
		Name: "inventory-svc",
		Host: "inv-prod-01",
		Bodies: []string{
			"stock level updated for sku %s",
			"out of stock for sku %s",
			"reindex started",
			"warehouse sync completed",
			"inventory reservation expired for sku %s",
			"backorder created for sku %s",
		},
		Attrs: []Attr{
			{"sku", "str"},
			{"warehouse.id", "str"},
			{"stock.qty", "int"},
			{"stock.reorder_level", "int"},
		},
		Strs: []string{"SKU-1001", "SKU-2033", "SKU-4017", "SKU-8812", "SKU-9044",
			"wh-east-1", "wh-west-2", "wh-eu-central-1"},
	},
	{
		Name: "worker-svc",
		Host: "worker-prod-05",
		Bodies: []string{
			"job dispatched to queue",
			"job %d failed after %d retries",
			"queue depth high (%d pending)",
			"processor heartbeat",
			"dead letter message consumed",
			"job %d completed in %d ms",
		},
		Attrs: []Attr{
			{"job.id", "int"},
			{"queue.name", "str"},
			{"job.retries", "int"},
			{"worker.id", "str"},
			{"job.duration_ms", "int"},
		},
		Strs: []string{"email-queue", "reindex-queue", "payout-queue", "worker-01", "worker-02", "worker-03"},
	},
}
