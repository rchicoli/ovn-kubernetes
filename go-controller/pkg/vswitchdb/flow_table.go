// Code generated by "libovsdb.modelgen"
// DO NOT EDIT.

package vswitchdb

const FlowTableTable = "Flow_Table"

type (
	FlowTableOverflowPolicy = string
)

var (
	FlowTableOverflowPolicyRefuse FlowTableOverflowPolicy = "refuse"
	FlowTableOverflowPolicyEvict  FlowTableOverflowPolicy = "evict"
)

// FlowTable defines an object in Flow_Table table
type FlowTable struct {
	UUID           string                   `ovsdb:"_uuid"`
	ExternalIDs    map[string]string        `ovsdb:"external_ids"`
	FlowLimit      *int                     `ovsdb:"flow_limit"`
	Groups         []string                 `ovsdb:"groups"`
	Name           *string                  `ovsdb:"name"`
	OverflowPolicy *FlowTableOverflowPolicy `ovsdb:"overflow_policy"`
	Prefixes       []string                 `ovsdb:"prefixes"`
}
