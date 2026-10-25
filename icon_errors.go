package main

import "net/http"

const iconErrorCodeHeader = "X-Favicon-Error-Code"

// Keep the existing English bodies and statuses. The browser reads only this
// fixed vocabulary, never an arbitrary error body or upstream diagnostic.
func iconErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_parameters"
	case http.StatusForbidden:
		return "source_forbidden"
	case http.StatusNotFound:
		return "source_not_found"
	case http.StatusRequestEntityTooLarge:
		return "resource_limit"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusServiceUnavailable:
		return "busy"
	case http.StatusGatewayTimeout:
		return "timeout"
	case http.StatusBadGateway:
		return "upstream_failure"
	default:
		return "generic"
	}
}

func writeIconError(w http.ResponseWriter, message string, status int) {
	w.Header().Set(iconErrorCodeHeader, iconErrorCode(status))
	http.Error(w, message, status)
}
