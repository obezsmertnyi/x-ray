package config

// DefaultNamespace is the namespace for X-Ray resources.
// Overridden at startup by the --namespace flag so multi-deployment
// releases use the correct namespace.
var DefaultNamespace = "xray"
