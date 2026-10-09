package doctor

// LetsEncryptDirectories returns the two ACME directory URLs of Let's Encrypt as doctor knows them, for the test
// that compares them with certmagic's.
func LetsEncryptDirectories() (production, staging string) {
	return letsEncryptProduction, letsEncryptStaging
}
