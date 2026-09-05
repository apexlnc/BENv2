// Package scenario defines #220's strict, developer-only orchestration
// scenario format and its deterministic diagnostic trace.
//
// The package describes inputs and renders observations. It does not execute
// the BEN authority loop or decide what any fact authorizes; internal/integration
// binds documents to the production orchestrator and its conformant fakes.
package scenario
