package service

// parentHealthyForShadow is intentionally a no-op until this fork imports the
// full upstream shadow-account model. Some upstream scheduler paths already
// call it; returning true preserves current local scheduling semantics without
// introducing partial shadow fields/methods that do not exist in this branch.
func parentHealthyForShadow(_ *Account, _ func(int64) *Account) bool {
	return true
}
