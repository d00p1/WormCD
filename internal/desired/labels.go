package desired

// Labels WormCD puts on every managed object. They are the only supported way
// to discover the objects of a target, and they must not be overridable from
// Compose files.
const (
	LabelApplication = "com.wormcd.application"
	LabelTarget      = "com.wormcd.target"
)

// Labels returns the ownership labels for an application target.
func Labels(application, target string) map[string]string {
	return map[string]string{
		LabelApplication: application,
		LabelTarget:      target,
	}
}

// MergeLabels copies base and adds (overwrites) the ownership labels.
func MergeLabels(base map[string]string, application, target string) map[string]string {
	out := make(map[string]string, len(base)+2)
	for k, v := range base {
		out[k] = v
	}
	for k, v := range Labels(application, target) {
		out[k] = v
	}
	return out
}
