package platformapi

import "net/http"

// GatewayStatuses are what a proxy in front of the platform answers in its place when it cannot
// reach it. Such an answer carries no JSON: one that does is the platform's own refusal. A client
// reads the platform as unreachable for these and for a request that got no answer at all.
var GatewayStatuses = []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
