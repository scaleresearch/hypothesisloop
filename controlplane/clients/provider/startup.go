package provider

// Sizer is a provider that can tell, renting nothing, whether it rents a size at all. A caller asks
// it when it starts, so a size the vendor never could rent is refused then and not at every Create.
type Sizer interface {
	// CheckSize refuses a spec whose cores, memory or disk no instance of the vendor has. Of spec
	// only Name and the sizes are read.
	CheckSize(spec Spec) error
}

// Accounted is a provider that rents on an account its settings name. With the provider's name
// the account is what stays the same of a cluster rented there when everything else changes.
type Accounted interface {
	Account() string
}

// ImageNeeder is a provider whose instances run something of its own beside the job's command,
// which needs programs in the job's image. A caller asks it when it starts and tells whoever
// builds an image.
type ImageNeeder interface {
	// ImageNeeds are the programs the job's image must have on its PATH.
	ImageNeeds() []string
}
