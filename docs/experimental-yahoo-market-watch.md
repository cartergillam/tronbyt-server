# Local experimental Yahoo Finance adapter

The optional Yahoo Finance route is a local experiment for the confirmed Plaza
Retail REIT TSX listing. It maps the display listing `PLZ.UN` to Yahoo's
`PLZ-UN.TO`, requires CAD, and returns the existing `MarketQuote` fields plus
`previousClose`. Daily change is calculated as `price - previousClose`; the
percentage is that difference divided by previous close. The quote timestamp
comes from Yahoo's `regularMarketTime`.

Yahoo's chart endpoint used here is unofficial, can change or block automated
clients without notice, and is not represented as a supported production API.
The response does not reliably establish a delay value or grant display,
redistribution, or commercial rights. The adapter therefore always marks
quotes `delayed: true` and leaves market state unknown. This is a conservative
UI label, not proof that a specific delay interval applies. Do not treat these
quotes as real-time or depend on them for financial decisions.

To enable locally, set all of:

```text
PRODUCTION=false
TRONBYT_LOCAL_DEVELOPMENT=true
TRONBYT_EXPERIMENTAL_YAHOO_FINANCE=true
```

The server otherwise keeps Twelve Data as its only provider. Even when enabled,
only a `PLZ.UN` request matching the TSX/XTSE CAD listing is sent to Yahoo;
other symbols continue through Twelve Data. A Yahoo failure is shown as a
sanitized per-symbol error or a stale last-known-good quote, never silently
replaced with a different listing. The standard quote cache, bounded stale
retention, rate-limit backoff, and owner diagnostic shape are reused. Yahoo
requires no credential and its cache identity contains only the verified
listing. Existing Twelve Data credentials remain required by the Market Watch
server flow for owner assignment and for all other configured symbols.

Automated tests use the checked-in response fixture and a local fake HTTP
server; they do not contact Yahoo. The fixture preserves the quote values
observed during the local yfinance proof of concept. Passing these tests proves
normalization and render-contract compatibility, not continued Yahoo endpoint
availability.

Production use would require a documented, stable provider interface and
written authorization covering automated retrieval, display on both private
household displays, caching, and any redistribution. If Yahoo does not grant
those rights, use a licensed provider. Any later production provider would
also need credential/ownership policy, operational monitoring, provider terms
review, and deployment approval; the local flags must not be enabled on the
production service.

Terms to review before any use beyond this local experiment:

- [Yahoo Terms of Service](https://legal.yahoo.com/us/en/yahoo/terms/otos/index.html)
- [Yahoo API Terms and Conditions](https://legal.yahoo.com/us/en/yahoo/terms/product-atos/apitnc/index.html)
