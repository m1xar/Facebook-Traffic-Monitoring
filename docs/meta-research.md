# Meta API Research Notes

This backend keeps the Meta API version configurable through `META_API_VERSION`.
As of June 10, 2026, Meta's public Graph API changelog lists `v25.0` released on
February 18, 2026, so new integrations should not hard-code older examples like
`v21.0`.

Research sources to re-check before production rollout:

- Graph API changelog: https://developers.facebook.com/docs/graph-api/changelog/
- Ads Insights API: https://developers.facebook.com/documentation/ads-commerce/marketing-api/insights
- Ad Account Insights reference: https://developers.facebook.com/documentation/ads-commerce/marketing-api/reference/ad-account/insights
- Rate limits: https://developers.facebook.com/docs/graph-api/overview/rate-limiting/

Implementation decisions:

- Use `META_API_VERSION` in all Graph/Marketing API URLs.
- Tokens are obtained through the server-side Facebook Login flow: the backend
  builds the OAuth dialog URL (`/api/fb-profiles/oauth/start`), receives the
  redirect at `/oauth/facebook/callback`, exchanges the authorization code for a
  short-lived token, then exchanges that for a long-lived (~60 day) token via
  `grant_type=fb_exchange_token` (requires `META_APP_SECRET`).
- The app stays in Development mode, so no App Review/business verification is
  needed, but only Meta app Testers/Developers can complete login.
- Long-lived user tokens cap at ~60 days; the scheduler warns 7 days before
  `token_expires_at` so the profile can be reconnected before sync breaks.
- Fetch cumulative `date_preset=today` snapshots, then derive hourly deltas before
  reports. Reports must never sum cumulative snapshots directly.
- Batch insight requests by profile/token with a maximum of 50 ad accounts per
  Graph batch request.
