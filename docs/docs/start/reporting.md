# Set up broker reporting

Updated: 2026-08-24

Canary needs one **Activity Flex Query** from Interactive Brokers. This is the
broker-reporting foundation for reconciliation, statement-derived equity,
Canary Edge, and future broker-truth analytics. A working Gateway connection
does not create this report for you.

The reliable setup is intentionally broad: add the nine sections below and
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

## 2. Add the nine reporting sections

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
| Currency Conversion Rate | All fields |

Leave **Filters** empty. A symbol filter would silently exclude account
activity Canary must reconcile. Selecting all fields is safe: Canary validates
the attributes it needs and ignores additional XML attributes.

### Exact XML attributes Canary validates

These are parser names, not always the labels shown in Client Portal. IBKR can
emit some of them automatically even when its current field reference does not
show a separate checkbox.

- `trades`: `accountId`, `assetCategory`, `currency`, `fxRateToBase`, `symbol`,
  `conid`, `underlyingConid`, `underlyingSymbol`, `multiplier`, `tradeID`,
  `ibOrderID`, `ibExecID`, `transactionID`, `tradeDate`, `tradeTime`, `buySell`,
  `quantity`, `tradePrice`, `proceeds`, `IBCommission`,
  `IBCommissionCurrency`, `taxes`, `openCloseIndicator`, `cost`,
  `fifoPnlRealized`, `mtmPnl`, `closePrice`, `netCash`, `levelOfDetail`.
- `instruments`: `assetCategory`, `currency`, `symbol`, `description`, `conid`,
  `underlyingConid`, `underlyingSymbol`, `multiplier`, `strike`, `expiry`,
  `putCall`, `listingExchange`.
- `open_positions`: `accountId`, `assetCategory`, `currency`, `fxRateToBase`,
  `symbol`, `conid`, `underlyingConid`, `underlyingSymbol`, `reportDate`,
  `position`, `multiplier`, `markPrice`, `costBasisPrice`, `costBasisMoney`,
  `fifoPnlUnrealized`, `side`, `openDateTime`.
- `option_events`: `accountId`, `assetCategory`, `currency`, `fxRateToBase`,
  `symbol`, `conid`, `underlyingConid`, `underlyingSymbol`, `date`,
  `transactionType`, `quantity`, `tradePrice`, `closePrice`, `proceeds`,
  `commissionsAndTax`, `cost`, `fifoPnlRealized`, `mtmPnl`, `tradeID`.
- `corporate_actions`: `accountId`, `assetCategory`, `currency`,
  `fxRateToBase`, `symbol`, `conid`, `underlyingConid`, `underlyingSymbol`,
  `multiplier`, `reportDate`, `dateTime`, `quantity`, `proceeds`, `amount`,
  `fifoPnlRealized`, `mtmPnl`, `type`, `transactionID`.
- `transfers`: `accountId`, `assetCategory`, `currency`, `fxRateToBase`,
  `symbol`, `conid`, `date`, `direction`, `quantity`, `cashTransfer`,
  `positionAmountInBase`, `transactionID`.
- `cash_transactions`: `transactionID`, `type`, `currency`, `fxRateToBase`,
  `amount`, `dateTime`, `settleDate`.
- `equity`: `reportDate`, `total`.
- `fx_rates`: `dateTime`, `fromCurrency`, `toCurrency`, `rate`.

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

## 5. Store the token without shell-history exposure

The commands below prompt silently. Your token is read into memory, written to
a file with mode `0600`, then removed from the shell variable. The token itself
never appears in the command line or history.

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

## 6. Restart and check Canary

```sh
canary restart
canary status
canary recon show
```

The first report may take time to generate. A temporary `report_not_ready`,
`service_busy`, `response_invalid`, or backfill state is retried without
changing your query. Credential reasons such as `token_expired`,
`token_invalid`, `query_invalid`, `service_inactive`, or `ip_restricted` need a
configuration correction.

Recon should eventually report an active/current statement source. Canary Edge
uses the same report and can remain `backfilling` while it requests its initial
365-day evidence window. `current` means the consumer has fresh, validated
broker evidence. `action_required` means Canary proved a credential or report
definition problem rather than merely waiting for IBKR.

An empty section is not proof that every field was selected. Canary calls that
section **unproved** until a real row arrives. Do not manufacture a trade,
transfer, or corporate action just to populate it.

## Fix an existing incomplete query safely

Do not edit the working query in place. Create a second Activity Flex Query
with the checklist above, validate it, then replace the Query ID in Canary.
Keep the old query until the new one is current. This avoids taking
reconciliation offline while correcting the report.

## Official IBKR references

- [Create an Activity Flex Query](https://www.ibkrguides.com/clientportal/performanceandstatements/activityflex.htm)
- [Activity Flex Query field reference](https://www.ibkrguides.com/reportingreference/reportguide/activity%20flex%20query%20reference.htm)
- [Find the Query ID](https://www.interactivebrokers.com/docs/web-api/flex-web-service/client-portal-configuration/create-a-flex-query)
- [Enable the Flex Web Service and create a token](https://www.interactivebrokers.com/docs/web-api/flex-web-service/client-portal-configuration/enable-and-create-access-token)
- [Flex Web Service date overrides and error behavior](https://www.ibkrguides.com/adminportal/performanceandstatements/flex3.htm)

For the local settings themselves, see the [configuration
reference](../reference/config.md). If a configured report still does not
advance, continue with [Troubleshooting](troubleshooting.md).
