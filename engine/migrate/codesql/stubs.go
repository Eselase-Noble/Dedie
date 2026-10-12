package codesql

import "errors"

// The other routes are not written yet. Convert turns them down before
// any of these is reached.

func parsePostgres(src string) (*routine, error) { return nil, errors.New("not converted yet") }
func (g *gen) emitMySQL() []string               { return nil }

var postgresMy map[string]fnRule
