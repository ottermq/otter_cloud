package instances

// Reasons are why an instance is in its status, or why an operation
// failed, as customers see them (portals.md §3). This is the one list: a
// worker sets a code from it, and the API shows the code with its message.
// Internal detail (Operation.last_error) never reaches a customer.
//
// The messages are written for a customer, say what to do next, and name
// nothing inside the platform: no node, cluster, step or error text.
var reasons = map[string]string{
	"create_failed":          "The database could not be created. Delete it and try again, or contact support.",
	"delete_failed":          "The database could not be deleted yet. The operator has been told and will finish it.",
	"start_failed":           "The database could not be started. Try again in a few minutes, or contact support.",
	"stop_failed":            "The database could not be stopped. Try again in a few minutes, or contact support.",
	"restart_failed":         "The database could not be restarted. Try again in a few minutes, or contact support.",
	"reset_password_failed":  "The password could not be reset. Your current password still works; try again in a few minutes.",
	"over_storage_quota":     "The database is over its storage allocation. Free some space, or delete data you no longer need.",
	"expired":                "The database reached its expiry date and was stopped.",
	"suspended":              "The database was suspended by the operator. Contact support.",
	"canceled":               "The operator canceled this operation.",
	"infrastructure_missing": "The database's data could not be found. The operator has been told and will contact you.",
}

// unknownReason is shown for a code missing from the list: a bug, but not
// one that should leak whatever the code was meant to stand for.
const unknownReason = "Something went wrong. Contact support."

// ReasonMessage is the customer-facing message for code; "" for no code.
func ReasonMessage(code string) string {
	if code == "" {
		return ""
	}
	if m, ok := reasons[code]; ok {
		return m
	}
	return unknownReason
}

// IsReason reports whether code is on the list.
func IsReason(code string) bool {
	_, ok := reasons[code]
	return ok
}
