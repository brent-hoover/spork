package trackerclient_test

import "errors"

func asAPIError(err error, target any) bool { return errors.As(err, target) }
