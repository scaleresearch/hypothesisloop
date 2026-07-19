package quota

import "net/http"

// donationHTTPStatus maps a DonationError reason to the appropriate HTTP status code.
func donationHTTPStatus(reason string) int {
	switch reason {
	case DonationReasonNotFound:
		return http.StatusNotFound
	case DonationReasonInsufficientQuota:
		return http.StatusPaymentRequired
	case DonationReasonInvalidState, DonationReasonSelfDonation:
		return http.StatusConflict
	default:
		return http.StatusUnprocessableEntity
	}
}
