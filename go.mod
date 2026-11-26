module ptscry

go 1.24

require golang.org/x/crypto v0.41.0

require golang.org/x/sys v0.39.0 // indirect

replace golang.org/x/crypto => ./third_party/crypto

replace golang.org/x/sys => ./third_party/sys
