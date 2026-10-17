package domain

// DataKey is one job's object-store key, never shared with another, and taken out of the store
// once its job ended (platformexperiments.DataKeySweep).
type DataKey struct {
	AccessKeyID     string
	SecretAccessKey string
}
