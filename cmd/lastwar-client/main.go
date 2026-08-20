// Command lastwar-client is the CLI entry point. All logic lives in internal/app (and the layered
// internal/sfs, internal/crypto, internal/gsl packages beneath it); this binary is a thin shim so
// the application code stays importable and testable as a library.
package main

import "lastwar-client/internal/app"

func main() { app.Run() }
