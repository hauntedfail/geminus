// Package logo holds the geminus logo for the program itself.
package logo

import _ "embed"

// PNG is the logo on a transparent square, 512 px.
//
//go:embed geminus-512.png
var PNG []byte
