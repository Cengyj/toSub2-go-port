package jshost

import _ "embed"

//go:embed dom.js
var domSource string

//go:embed network.js
var networkSource string

//go:embed clone.js
var cloneSource string

//go:embed workers.js
var workerSource string
