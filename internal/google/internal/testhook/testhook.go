// Package testhook is how googletest points a google.API at a fake. Go's
// internal rule lets only internal/google and packages under it import
// this, so nothing else in pneu (no config, no environment) can move the
// three Google hosts anywhere.
package testhook

// WithBase, set by package google, serves api's three hosts at
// base + "/" + host (the fake tells them apart by that prefix). api is a
// *google.API.
var WithBase func(api any, base string)
