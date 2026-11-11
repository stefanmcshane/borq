package apps

import "errors"

type postgresValidationError struct {
	message string
}

func (err postgresValidationError) Error() string { return err.message }

func (err postgresValidationError) Unwrap() error { return ErrInvalid }

type postgresNameConflictError struct{}

func (err postgresNameConflictError) Error() string {
	return "A PostgreSQL database with this name already exists in this space. Choose a new name to create or restore a database, or use apply to update the existing database."
}

func (err postgresNameConflictError) Unwrap() error { return ErrConflict }

// PostgresRequestMessage returns approved PostgreSQL guidance, ignoring surrounding
// error text which may contain private values.
func PostgresRequestMessage(err error) string {
	var validation postgresValidationError
	if errors.As(err, &validation) {
		return validation.message
	}
	var conflict postgresNameConflictError
	if errors.As(err, &conflict) {
		return conflict.Error()
	}
	return ""
}
