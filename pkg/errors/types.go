package errors

// BadRequest new BadRequest error that is mapped to a 400 response.
//func BadRequest(reason, message string) *Error {
//	return Newf(400, reason, message)
//}

func BadRequest(reason, cnMessage, enMessage string) *JyError {
	return &JyError{
		reason:    reason,
		CnMessage: cnMessage,
		EnMessage: enMessage,
	}
}

// IsBadRequest determines if err is an error which indicates a BadRequest error.
// It supports wrapped errors.
func IsBadRequest(err error) bool {
	return Code(err) == 400
}

// Unauthorized new Unauthorized error that is mapped to a 401 response.
func Unauthorized(reason, message string) *Error {
	return Newf(401, reason, message)
}

// IsUnauthorized determines if err is an error which indicates a Unauthorized error.
// It supports wrapped errors.
func IsUnauthorized(err error) bool {
	return Code(err) == 401
}

// Forbidden new Forbidden error that is mapped to a 403 response.
func Forbidden(reason, message string) *Error {
	return Newf(403, reason, message)
}

// IsForbidden determines if err is an error which indicates a Forbidden error.
// It supports wrapped errors.
func IsForbidden(err error) bool {
	return Code(err) == 403
}

// NotFound new NotFound error that is mapped to a 404 response.
func NotFound(reason, message string) *Error {
	return Newf(404, reason, message)
}

// IsNotFound determines if err is an error which indicates an NotFound error.
// It supports wrapped errors.
func IsNotFound(err error) bool {
	return Code(err) == 404
}

// Conflict new Conflict error that is mapped to a 409 response.
func Conflict(reason, message string) *Error {
	return Newf(409, reason, message)
}

// IsConflict determines if err is an error which indicates a Conflict error.
// It supports wrapped errors.
func IsConflict(err error) bool {
	return Code(err) == 409
}

// InternalServer new InternalServer error that is mapped to a 500 response.
func InternalServer(reason, message string) *Error {
	return Newf(500, reason, message)
}

// IsInternalServer determines if err is an error which indicates an Internal error.
// It supports wrapped errors.
func IsInternalServer(err error) bool {
	return Code(err) == 500
}

// ServiceUnavailable new ServiceUnavailable error that is mapped to a HTTP 503 response.
func ServiceUnavailable(reason, message string) *Error {
	return Newf(503, reason, message)
}

// IsServiceUnavailable determines if err is an error which indicates a Unavailable error.
// It supports wrapped errors.
func IsServiceUnavailable(err error) bool {
	return Code(err) == 503
}

// GatewayTimeout new GatewayTimeout error that is mapped to a HTTP 504 response.
func GatewayTimeout(reason, message string) *Error {
	return Newf(504, reason, message)
}

// IsGatewayTimeout determines if err is an error which indicates a GatewayTimeout error.
// It supports wrapped errors.
func IsGatewayTimeout(err error) bool {
	return Code(err) == 504
}

// ClientClosed new ClientClosed error that is mapped to a HTTP 499 response.
func ClientClosed(reason, message string) *Error {
	return Newf(499, reason, message)
}

// IsClientClosed determines if err is an error which indicates a IsClientClosed error.
// It supports wrapped errors.
func IsClientClosed(err error) bool {
	return Code(err) == 499
}
