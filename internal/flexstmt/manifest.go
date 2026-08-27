package flexstmt

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// ManifestVersion identifies the single Flex query profile understood by the
// statement parser and the Recon and Edge reporting consumers. It is
// deliberately not a setting: setup copy, diagnostics, generated
// documentation, and parser coverage all consume this same manifest.
const ManifestVersion = "canary-reporting-flex-v2"

// ManifestSection describes one section of the canonical Activity Flex Query.
// Fields use IBKR's XML attribute names so the list can be compared directly
// with retained broker evidence.
type ManifestSection struct {
	Key            string
	Label          string
	Container      string
	Row            string
	LevelOfDetail  string
	RequiredFields []string
}

// Query requirement evidence states distinguish what broker XML proves from
// what an empty section cannot prove.
const (
	QueryRequirementObserved    = "observed"
	QueryRequirementMissing     = "missing"
	QueryRequirementAbsent      = "absent"
	QueryRequirementEmpty       = "empty"
	QueryRequirementNotReceived = "not_received"
)

// QuerySectionEvidence is one sanitized comparison against the canonical
// manifest. MissingFields contains only canonical field names; broker values
// and free text never enter this type.
type QuerySectionEvidence struct {
	Key           string
	Status        string
	MissingFields []string
}

var canonicalManifest = []ManifestSection{
	{
		Key: "trades", Label: "Trades", Container: "Trades", Row: "Trade", LevelOfDetail: "Executions",
		RequiredFields: []string{
			"accountId", "assetCategory", "currency", "fxRateToBase", "symbol", "conid",
			"underlyingConid", "underlyingSymbol", "multiplier", "tradeID", "ibOrderID",
			"ibExecID", "transactionID", "dateTime", "tradeDate", "buySell",
			"quantity", "tradePrice", "proceeds", "ibCommission", "ibCommissionCurrency",
			"taxes", "openCloseIndicator", "cost", "fifoPnlRealized", "mtmPnl", "closePrice",
			"netCash", "levelOfDetail",
		},
	},
	{
		Key: "instruments", Label: "Financial Instrument Information", Container: "SecuritiesInfo", Row: "SecurityInfo",
		RequiredFields: []string{
			"assetCategory", "currency", "symbol", "description", "conid", "underlyingConid",
			"underlyingSymbol", "multiplier", "strike", "expiry", "putCall", "listingExchange",
		},
	},
	{
		Key: "open_positions", Label: "Open Positions", Container: "OpenPositions", Row: "OpenPosition", LevelOfDetail: "Summary",
		RequiredFields: []string{
			"accountId", "assetCategory", "currency", "fxRateToBase", "symbol", "conid",
			"underlyingConid", "underlyingSymbol", "reportDate", "position", "multiplier",
			"markPrice", "costBasisPrice", "costBasisMoney", "fifoPnlUnrealized", "side", "openDateTime",
		},
	},
	{
		Key: "option_events", Label: "Options, Exercises, Assignments and Expirations", Container: "OptionEAE", Row: "OptionEAE",
		RequiredFields: []string{
			"accountId", "assetCategory", "currency", "fxRateToBase", "symbol", "conid",
			"underlyingConid", "underlyingSymbol", "date", "transactionType", "quantity",
			"tradePrice", "markPrice", "proceeds", "commisionsAndTax", "costBasis", "realizedPnl", "mtmPnl", "tradeID",
		},
	},
	{
		Key: "corporate_actions", Label: "Corporate Actions", Container: "CorporateActions", Row: "CorporateAction",
		RequiredFields: []string{
			"accountId", "assetCategory", "currency", "fxRateToBase", "symbol", "conid",
			"underlyingConid", "underlyingSymbol", "multiplier", "reportDate", "dateTime",
			"quantity", "proceeds", "amount", "fifoPnlRealized", "mtmPnl", "type", "transactionID",
		},
	},
	{
		Key: "transfers", Label: "Transfers", Container: "Transfers", Row: "Transfer",
		RequiredFields: []string{
			"accountId", "assetCategory", "currency", "fxRateToBase", "symbol", "conid",
			"date", "direction", "quantity", "cashTransfer", "positionAmountInBase", "transactionID",
		},
	},
	{
		Key: "cash_transactions", Label: "Cash Transactions", Container: "CashTransactions", Row: "CashTransaction",
		RequiredFields: []string{
			"transactionID", "type", "currency", "fxRateToBase", "amount", "dateTime", "settleDate",
		},
	},
	{
		Key: "equity", Label: "Net Asset Value (NAV) Summary in Base", Container: "EquitySummaryInBase", Row: "EquitySummaryByReportDateInBase",
		RequiredFields: []string{"reportDate", "total"},
	},
}

// CanonicalQueryManifest returns a defensive copy of the one supported query
// profile. Callers may format it, but cannot mutate parser authority.
func CanonicalQueryManifest() []ManifestSection {
	out := make([]ManifestSection, len(canonicalManifest))
	for i, section := range canonicalManifest {
		out[i] = section
		out[i].RequiredFields = append([]string(nil), section.RequiredFields...)
	}
	return out
}

// SetupSteps are intentionally short enough for the CLI, MCP, and app to use
// verbatim. The field list itself always comes from CanonicalQueryManifest.
func SetupSteps() []string {
	return []string{
		"Open the saved XML Activity Flex Query in IBKR Client Portal and add every Canary reporting section shown below.",
		"For each section choose Select All; keep Trades at Executions detail and Open Positions at Summary detail, then save.",
		"Run canary setup reporting to validate it. Canary names sections that were absent, empty, or missing fields and explains the repair.",
	}
}

type observedManifestSection struct {
	present bool
	rows    int
	fields  map[string]bool
}

func observeManifestSections(statements []Statement) map[string]*observedManifestSection {
	observed := make(map[string]*observedManifestSection, len(canonicalManifest))
	for _, statement := range statements {
		for _, coverage := range statement.Coverage {
			section := observed[coverage.Key]
			if section == nil {
				section = &observedManifestSection{fields: map[string]bool{}}
				observed[coverage.Key] = section
			}
			section.present = section.present || coverage.Present
			section.rows += coverage.RowCount
			for _, field := range coverage.ObservedFields {
				section.fields[field] = true
				if alias := canonicalObservedField(coverage.Key, field); alias != "" {
					section.fields[alias] = true
				}
			}
		}
	}
	return observed
}

// canonicalObservedField maps historical IBKR execution aliases onto the
// current Portal XML names. Parsing remains backward compatible with retained
// statements, while setup and diagnostics teach only the fields users can
// select today.
func canonicalObservedField(sectionKey, field string) string {
	if sectionKey != "trades" {
		return ""
	}
	switch field {
	case "tradeTime":
		return "dateTime"
	case "IBCommission":
		return "ibCommission"
	case "IBCommissionCurrency":
		return "ibCommissionCurrency"
	default:
		return ""
	}
}

// QueryRequirementEvidence compares actual retained broker XML with the
// canonical manifest. An absent container and a present-but-empty container
// are separate evidence states: neither proves its selected fields, but only
// the former tells the operator that the section was not returned at all.
func QueryRequirementEvidence(statements []Statement) []QuerySectionEvidence {
	observed := observeManifestSections(statements)
	evidence := make([]QuerySectionEvidence, 0, len(canonicalManifest))
	for _, required := range canonicalManifest {
		row := QuerySectionEvidence{Key: required.Key}
		if len(statements) == 0 {
			row.Status = QueryRequirementNotReceived
			evidence = append(evidence, row)
			continue
		}
		section := observed[required.Key]
		switch {
		case section == nil || !section.present:
			row.Status = QueryRequirementAbsent
		case section.rows == 0:
			row.Status = QueryRequirementEmpty
		default:
			for _, field := range required.RequiredFields {
				if !section.fields[field] {
					row.MissingFields = append(row.MissingFields, field)
				}
			}
			if len(row.MissingFields) > 0 {
				row.Status = QueryRequirementMissing
			} else {
				row.Status = QueryRequirementObserved
			}
		}
		evidence = append(evidence, row)
	}
	return evidence
}

// QuerySchemaFingerprint identifies only the observed canonical schema, not
// statement values, account identity, activity counts, or filenames.
func QuerySchemaFingerprint(statements []Statement) string {
	if len(statements) == 0 {
		return ""
	}
	var canonical strings.Builder
	canonical.WriteString(ManifestVersion)
	canonical.WriteByte('\n')
	for _, section := range QueryRequirementEvidence(statements) {
		fields := append([]string(nil), section.MissingFields...)
		sort.Strings(fields)
		fmt.Fprintf(&canonical, "%s|%s|%s\n", section.Key, section.Status, strings.Join(fields, ","))
	}
	digest := sha256.Sum256([]byte(canonical.String()))
	return fmt.Sprintf("flex_schema_%x", digest[:8])
}

// MissingQueryRequirements reports required fields proven absent by non-empty
// rows. Absent and empty sections remain separate, unproved evidence states.
// Values are stable manifest keys; broker text never enters the diagnostic.
func MissingQueryRequirements(statements []Statement) []string {
	missing := []string{}
	for _, section := range QueryRequirementEvidence(statements) {
		if section.Status != QueryRequirementMissing {
			continue
		}
		for _, field := range section.MissingFields {
			missing = append(missing, section.Key+"."+field)
		}
	}
	return missing
}
