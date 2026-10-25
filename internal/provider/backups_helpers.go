package provider

import (
	"errors"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The API's own limits (packages/shared/src/export-destinations.ts), repeated
// here so a value the API would refuse is refused at plan instead of halfway
// through an apply.
var (
	awsAccountIDRe      = regexp.MustCompile(`^\d{12}$`)
	exportBucketRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	exportPrefixRe      = regexp.MustCompile(`^$|^[A-Za-z0-9!_.*'()/-]{1,200}/$`)
	exportRoleNameRe    = regexp.MustCompile(`^KiviDBManagement-Exports[A-Za-z0-9+=,.@_-]*$`)
	noOuterSpaceRe      = regexp.MustCompile(`(?s)^\S(.*\S)?$`)
	kdbExportCadences   = []string{"off", "daily", "weekly"}
	defaultExportPrefix = "kividb/"
)

// How long the backup resources wait, and how often they look. Variables so
// the tests can run in milliseconds.
var (
	verifyRetryFor     = 5 * time.Minute
	verifyRetryEvery   = 10 * time.Second
	kdbSettleFor       = 60 * time.Minute
	kdbSettleEvery     = 10 * time.Second
	kdbDeleteWaitFor   = 30 * time.Minute
	kdbDeleteWaitEvery = 10 * time.Second
)

func isBackupNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsNotFound()
}

// explainBackup adds words to the refusals the backup endpoints send with a
// code and no message. Anything with a message is used as the server wrote it.
func explainBackup(err error) string {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "" {
		return explain(err)
	}
	switch apiErr.Code {
	case "instance_not_running":
		return "The database is not running. A KDB export reads the running database; start it and apply again."
	case "snapshot_in_progress":
		return "The export is still being written and cannot be deleted until it finishes."
	case "not_found":
		return "Not found: it was deleted, or the API key cannot see it (a key limited to some databases " +
			"cannot reach export destinations at all)."
	}
	return explain(err)
}

func int64OrNull(f flexFloat) types.Int64 {
	if f.Value == nil {
		return types.Int64Null()
	}
	return types.Int64Value(int64(*f.Value))
}
