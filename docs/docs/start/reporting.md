# Set up broker reporting

Updated: 2026-08-25

Canary needs one **Activity Flex Query** from Interactive Brokers. This is the
broker-reporting foundation for reconciliation, statement-derived equity,
Canary Edge, and future broker-truth analytics. A working Gateway connection
does not create this report for you.

The reliable setup is intentionally broad: add the eight required sections below and
select **all fields** in each one. Canary ignores fields it does not use. This
is easier to follow and more resilient to IBKR label changes than asking you to
find individual XML attributes in the Portal.

This setup is read-only. It lets Canary retrieve statements; it grants no order
or account-management authority.

## Before you start

You need:

- access to the IBKR Client Portal for the account Canary uses;
- the exact account selected when your login manages more than one account;
- a text editor and a terminal on the machine running Canary.

Keep the Flex token private. Do not paste it into a shell command, issue,
support message, screenshot, or Canary's TOML file.

## 1. Open Activity Flex Queries

In Client Portal, choose **Performance & Reports → Flex Queries**. The alternate
path is **Menu → Reporting → Flex Queries**.

If an Account Selector opens, select only the account Canary is configured to
use. IBKR shows a saved query again only when the same account selection is
active. For advisor or linked-account structures, the Flex Web Service token is
normally visible from the master account even when the query itself is scoped
to one subaccount.

![IBKR Flex Queries page](https://www.ibkrguides.com/clientportal/resources/images/flex.jpg)

*IBKR's Flex Queries page. Screenshot: Interactive Brokers Client Portal User
Guide.*

Under **Activity Flex Query**, click the **+** button. Do not use Trade
Confirmation Flex Query: it does not contain the statement sections Canary
needs.

![Create an Activity Flex Query](https://www.ibkrguides.com/clientportal/resources/images/create%20activity%20flex%20query.png)

*The Activity Flex Query editor. Screenshot: Interactive Brokers Client Portal
User Guide.*

Give the query a recognizable name, such as `Canary Reporting`.

If your Portal shows **Configure with AI**, IBKR also supports an interactive draft workflow. A useful starting prompt is:

```text
Create an XML Activity Flex Query for one selected account and the last 365 calendar days, with no symbol or model filters. Include all fields in Trades at Executions detail, Financial Instrument Information, Open Positions at Summary detail, Options/Exercises/Assignments/Expirations, Corporate Actions, Transfers, Cash Transactions, and NAV Summary in Base.
```

Use **Edit Manually** to verify every section and field below before saving. IBKR says AI-generated Flex output may be incomplete or inaccurate, and delivery configuration still belongs on the ordinary Flex Queries page. This path reduces clicking; it does not replace Canary's deterministic validation.

## 2. Add the eight required reporting sections

Add each section below. When a section opens its field picker, choose **Select
All** or check every available field, then save the section.

| Section in IBKR Client Portal | Required detail |
| --- | --- |
| Trades | Executions |
| Financial Instrument Information | All fields |
| Open Positions | Summary |
| Options, Exercises, Assignments and Expirations | All fields |
| Corporate Actions | All fields |
| Transfers (ACATS, Internal) | All fields |
| Cash Transactions | All fields |
| Net Asset Value (NAV) Summary in Base | All fields |

Leave **Filters** empty. A symbol filter would silently exclude account
activity Canary must reconcile. Selecting all fields is safe: Canary validates
the attributes it needs and ignores additional XML attributes.

Some account and Client Portal variants also show **Currency Conversion Rate**.
If it is available, add it and select all fields; it improves dated FX evidence
for non-base-currency Edge horizons. It is not required because IBKR does not
offer it in every live section picker. **Forex Balances** and **Forex P/L
Details** are different reports and are not substitutes. Canary will leave an
affected non-base-currency result unscored when the required broker FX evidence
is unavailable.

The exact XML attribute names are generated from the parser itself in the
[Canary reporting Flex query reference](../reference/edge-flex.md). Use that
page when diagnosing a named missing requirement. Do not copy the list into a
separate checklist: the generated reference is the field authority.

## 3. Configure delivery

Under **Delivery Configuration**:

1. **Accounts:** use Add/Edit Accounts and choose the one Canary account. Do
   not use a consolidated multi-account report.
2. **Models:** leave models unrestricted unless the whole account genuinely
   uses one model.
3. **Format:** XML.
4. **Period:** Last 365 Calendar Days. Canary can use IBKR's supported date
   override for later incremental and replay fetches, but this default also
   makes a manual test useful.

![IBKR Flex delivery configuration](https://www.ibkrguides.com/clientportal/resources/images/deliveryconfiguration.jpg)

*Delivery Configuration. Screenshot: Interactive Brokers Client Portal User
Guide.*

Under **General Configuration**, choose a machine-readable date/time format:
`yyyyMMdd`, 24-hour time, and `;` as the date/time separator. Keep the real
Account ID in the XML rather than substituting an account alias.

![IBKR Flex general configuration](https://www.ibkrguides.com/clientportal/resources/images/generalconfig.jpg)

*General Configuration. Screenshot: Interactive Brokers Client Portal User
Guide.*

Click **Continue**, review the query, then click **Create**. Back on the Flex
Queries page, click the query's information icon and record its Query ID.

## 4. Enable the Flex Web Service

Open **Flex Web Service Configuration** on the Flex Queries page. Enable **Flex
Web Service Status** and save it. Generate a token with an expiry appropriate
for a long-running local service. An IP restriction is optional, but if you use
one it must allow the machine running Canary.

Generating a new token invalidates the current token. Do not rotate it while a
working Canary installation still depends on the old value unless you are
ready to update Canary immediately.

![IBKR Flex Web Service configuration](https://www.ibkrguides.com/adminportal/resources/images/configure%20flex%20web%202.png)

*Flex Web Service Configuration. Screenshot: Interactive Brokers Portal User
Guide. Token values in documentation are illustrative; never share yours.*

## 5. Validate and activate it with Canary

Run the interactive setup wizard on the machine that runs Canary:

```sh
canary setup reporting
```

The wizard prints the canonical checklist, asks for the Query ID, and reads the
token with terminal echo disabled. Paste with your terminal's normal Paste
command—**Command-V on macOS**—then press Return. No characters or bullets
appear while you paste. Canary confirms `Token received` only after Return; it
never prints the value. It writes the token to a private `0600` file, asks the
daemon to fetch and validate the candidate report, and changes the active
configuration only after that check succeeds. Candidate XML is parsed in
memory and discarded: it is not retained, projected, logged, or returned by
the local API.

A date range with no trades, transfers, or corporate actions cannot prove the
fields selected for those empty sections. The wizard calls them **unproved**
and asks before activating the candidate. It refuses activation when a
non-empty section proves that a named field is missing.

For unattended recovery, `canary setup reporting --accept-unproved` accepts
only that empty-section uncertainty. It never accepts a field that a returned
row proves missing. Use the flag only after comparing the Portal definition
with the generated field reference.

On success, Canary updates the `[flex]` settings atomically, keeps one private
local config backup for rollback, restarts the daemon, and prints the shared
reporting state. If another query already works, this is a blue/green rotation:
the old Query ID and token remain active until the candidate passes validation.
After activation, Canary gives the replacement Query ID its own opaque evidence
generation. Old XML remains available for audit but cannot make the replacement
look complete or feed its Recon and Edge results. XML retained before query
generation tags existed is also preserved but must be refetched by the active
query before Canary treats it as current evidence.

Canary also removes superseded `flex-token-*` files from the config directory after a successful promotion, retaining only the active token and the one token referenced by the rollback config. A relative `CANARY_CONFIG` is resolved to an absolute path before the token is created or the daemon is restarted.

### Why Canary cannot create the query for you

IBKR's documented Flex Web Service exposes two operations: trigger a report for a saved Query ID and retrieve that generated report. Query definition remains an authenticated Portal workflow. The classic editor and Configure with AI both end in an explicit preview/save step; neither is documented as query-definition CRUD that Canary can call. Canary therefore verifies everything the returned XML can prove, but it never replays Portal mutations or claims an undocumented creation API exists. See IBKR's [Flex API reference](https://www.interactivebrokers.com/docs/web-api/api-reference/send-request), [classic Activity Flex workflow](https://www.ibkrguides.com/brokerportal/performanceandstatements/activityflex.htm), and [Configure Flex with AI guide](https://www.ibkrguides.com/complianceportal/configure-flex-with-ai.htm).

### Manual configuration fallback

If you need to configure an installation that cannot run the interactive
wizard, store the token without shell-history exposure. The commands below
prompt silently. Your token is read into memory, written to a file with mode
`0600`, then removed from the shell variable. The token itself never appears in
the command line or history.

```zsh
mkdir -p ~/.config/ibkr
chmod 700 ~/.config/ibkr
read -rs 'flex_token?Paste Flex token: '; printf '\n'
(umask 077; printf '%s\n' "$flex_token" > ~/.config/ibkr/flex-token)
unset flex_token
chmod 600 ~/.config/ibkr/flex-token
```

Open `~/.config/ibkr/config.toml` in an editor and add:

```toml
[flex]
enabled = true
query_id = "YOUR_QUERY_ID"
token_path = "~/.config/ibkr/flex-token"
```

The Query ID selects a saved report. The token authorizes statement retrieval.
Only the token belongs in the protected token file.

## 6. Check reporting, Recon, and Edge

```sh
canary restart
canary reporting status
canary recon show
canary edge
```

The restart loads manually edited files. The wizard performs it for you unless
you passed `--no-restart`. Then `reporting status` separates local
credential-file problems from broker and report evidence. Use these states:

| Reporting state | Meaning |
| --- | --- |
| `configured` | Local setup is sound, but one or more empty sections are still unproved. |
| `backfilling` | Canary is waiting for its first usable report or building the requested history. Temporary broker reasons are retried automatically. |
| `current` | Fresh broker evidence satisfies every requirement that the returned XML can prove. |
| `action_required` | Canary proved a local credential problem, a named report requirement is absent, or IBKR returned a response that needs operator attention. |
| `unavailable` | Canary cannot read its local reporting authority. |

`canary reporting status --json` reports local readiness, broker reachability,
freshness, the reporting-manifest version, an account-free schema fingerprint,
named missing requirements, and unproved sections. It never returns the token,
Query ID, account ID, statement filename, holdings, balances, or raw XML.

Temporary `report_not_ready`, `service_busy`, `rate_limited`, or
`network_unavailable` reasons are retried without changing your query.
Credential reasons such as `token_expired`, `token_invalid`, `query_invalid`,
`service_inactive`, or `ip_restricted` need a configuration correction.

Canary asks Flex for a range ending on the latest completed New York reporting
date, never the still-open reporting date. IBKR code `1003` means no statement
is available for that completed date. Canary retries it conservatively; if it
persists after the next statement publication, review the query's account
selection and availability rather than regenerating the token blindly.

IBKR may also return numeric statuses not present in its published Flex error
table. Canary exposes only the four-digit code. For example, code `1025` is
currently undocumented: review the Flex Web Service and query configuration,
then contact IBKR if it persists. Canary does not guess what undocumented
broker text means.

Recon should eventually report an active/current statement source. Canary Edge
uses the same report and can remain `backfilling` while it requests its initial
365-day evidence window. The four report chunks are paced one minute apart, then exact-contract daily bars are collected sequentially, so the first decision review normally takes several minutes after a healthy setup; broker retry states expose when Canary will try again. `current` means the consumer has fresh, validated
broker evidence. `action_required` means Canary proved a credential or report
definition problem rather than merely waiting for IBKR.

`insufficient_evidence` is an Edge state, not a Reporting state. It means account-level evidence may be usable but the returned reports did not prove the Trades section. Run `canary reporting status` (or the read-only `canary_reporting` MCP tool) and check the saved Portal query. If Trades is present and explicitly empty, Edge instead keeps a `current` zero-decision result with a plain explanation.

An empty section is not proof that every field was selected. Canary calls that
section **unproved** until a real row arrives. Do not manufacture a trade,
transfer, or corporate action just to populate it.

## Fix an existing incomplete query safely

Do not edit the working query in place. Create a second Activity Flex Query
with the checklist above, then run `canary setup reporting` with its new Query
ID and token. Canary validates and promotes the candidate without taking the
working query offline. Keep the old Portal query until the replacement is
current.

## Official IBKR references

- [Create an Activity Flex Query](https://www.ibkrguides.com/clientportal/performanceandstatements/activityflex.htm)
- [Configure Flex with AI](https://www.ibkrguides.com/complianceportal/configure-flex-with-ai.htm)
- [Flex Web Service API reference](https://www.interactivebrokers.com/docs/web-api/api-reference/send-request)
- [Activity Flex Query field reference](https://www.ibkrguides.com/reportingreference/reportguide/activity%20flex%20query%20reference.htm)
- [Find the Query ID](https://www.interactivebrokers.com/docs/web-api/flex-web-service/client-portal-configuration/create-a-flex-query)
- [Enable the Flex Web Service and create a token](https://www.interactivebrokers.com/docs/web-api/flex-web-service/client-portal-configuration/enable-and-create-access-token)
- [Flex Web Service date overrides and error behavior](https://www.ibkrguides.com/adminportal/performanceandstatements/flex3.htm)

For the local settings themselves, see the [configuration
reference](../reference/config.md). If a configured report still does not
advance, continue with [Troubleshooting](troubleshooting.md).
