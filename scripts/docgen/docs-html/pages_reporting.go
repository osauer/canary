package main

// Register reporting immediately after installation: a connected Gateway is
// not enough for statement-backed features, so this belongs in the first-run
// path rather than under an individual consumer such as Recon or Edge.
func init() {
	reporting := pageSpec{
		Source:      "docs/docs/start/reporting.md",
		Section:     "start",
		NavTitle:    "Set up broker reporting",
		Summary:     "Create the shared IBKR Activity Flex Query, store its token safely, and prove statement-backed features are current.",
		Description: "How to configure the IBKR Activity Flex Query shared by Canary reconciliation, statement equity, and Edge; securely store the Flex token; and diagnose backfill or schema problems.",
		Status:      statusPublished,
	}

	insertAt := len(pages)
	for i, page := range pages {
		if page.Source == "docs/docs/start/install.md" {
			insertAt = i + 1
			break
		}
	}
	pages = append(pages, pageSpec{})
	copy(pages[insertAt+1:], pages[insertAt:])
	pages[insertAt] = reporting
}
