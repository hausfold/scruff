module github.com/hausfold/scruff/sdk/go

go 1.23

// v1.3.61 is a typo for v1.3.6 and is not a release. Use v1.4.0 or later.
//
// Every other registry scruff publishes to can yank a version. The Go module
// mirror cannot forget one, and it reads retractions from the go.mod of the
// highest version it holds, so this line does nothing until a tag above
// v1.3.61 carries it. That tag is v1.4.0.
retract v1.3.61
