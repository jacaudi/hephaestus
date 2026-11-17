// Package store keeps the utility boot artifacts in an OCI image layout on
// disk: layer blobs pushed under verification, manifests tagged per catalog
// entry and pin, size checks that tell a complete layer from a truncated one,
// and an Open that sets a corrupt layout aside instead of refusing to serve.
package store
