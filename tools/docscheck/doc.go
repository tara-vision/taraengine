// Package docscheck holds the tests that keep the documents of the repository in step with what they describe: the
// catalog table of the README with the embedded registry, so that a model added to, changed in or removed from
// internal/registry/registry.yaml changes the README in the same pull request, and the NOTICE with the license text
// that it has to carry. go test ./... says so when they drift apart.
package docscheck
