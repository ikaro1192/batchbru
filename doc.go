// Package batchbru manages AWS Batch job definitions as code: definitions
// written in JSON, Jsonnet or YAML are rendered, diffed against the latest
// ACTIVE revision in AWS, and registered as new revisions only when they
// differ.
//
// The package is the implementation of the batchbru command line tool; Run is
// its entry point. See the README for the commands and the configuration file
// format.
package batchbru
