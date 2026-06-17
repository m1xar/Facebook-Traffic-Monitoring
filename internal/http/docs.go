package apihttp

import "net/http"

func (s *Server) registerDocs() {
	s.router.Get("/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write([]byte(openAPIYAML))
	})
	s.router.Get("/swagger", s.swaggerUI)
	s.router.Get("/swagger/", s.swaggerUI)
}

func (s *Server) swaggerUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(swaggerHTML))
}

const swaggerHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Meta Tracking API Swagger</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>
    body { margin: 0; background: #f7f7f7; }
    .swagger-ui .topbar { display: none; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: '/openapi.yaml',
      dom_id: '#swagger-ui',
      deepLinking: true,
      presets: [SwaggerUIBundle.presets.apis],
      layout: 'BaseLayout'
    });
  </script>
</body>
</html>`

const openAPIYAML = `openapi: 3.0.3
info:
  title: Meta Tracking Backend API
  version: 2.0.0
  description: >
    Go backend for admin-connected Meta profiles, hourly cumulative snapshots of
    ad-account daily stats, and buyer attribution with ownership history.
servers:
  - url: /
security:
  - bearerAuth: []
paths:
  /healthz:
    get:
      security: []
      summary: Health check
      responses:
        "200":
          description: API is alive
          content:
            application/json:
              schema:
                type: object
                properties:
                  status:
                    type: string
                    example: ok
  /api/auth/register:
    post:
      security: []
      summary: Bootstrap the first admin account (works only on an empty database)
      description: Creates the initial admin when no users exist yet. After that it always returns 403; new accounts are created by an admin via POST /api/users or POST /api/buyers.
      tags: [Auth]
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/RegisterRequest"
      responses:
        "201":
          description: Bootstrap admin created
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/AuthResponse"
        "400":
          description: Invalid email or password
        "403":
          description: Users already exist, registration is disabled
        "409":
          description: Email already exists
  /api/auth/login:
    post:
      security: []
      summary: Login by email and password
      tags: [Auth]
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/LoginRequest"
      responses:
        "200":
          description: Valid access and refresh tokens
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/AuthResponse"
        "401":
          description: Invalid email or password
  /api/auth/refresh:
    post:
      security: []
      summary: Rotate refresh token and return a new token pair
      tags: [Auth]
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [refresh_token]
              properties:
                refresh_token:
                  type: string
      responses:
        "200":
          description: New token pair
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/TokenPair"
        "401":
          description: Invalid refresh token
  /api/users:
    post:
      summary: Create admin or buyer account (admin only)
      tags: [Auth]
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/RegisterRequest"
      responses:
        "201":
          description: Account created
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/AppUser"
        "400":
          description: Invalid email, password, role, or buyer_id
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
        "409":
          description: Email already exists
  /api/buyers:
    post:
      summary: Create buyer with CRM login credentials (admin only)
      description: Creates the buyer and their email+password login in one transaction.
      tags: [Buyers]
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/CreateBuyerRequest"
      responses:
        "201":
          description: Buyer and login created
          content:
            application/json:
              schema:
                type: object
                properties:
                  buyer:
                    $ref: "#/components/schemas/Buyer"
                  user:
                    $ref: "#/components/schemas/AppUser"
        "400":
          description: Missing display_name or invalid email/password
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
        "409":
          description: Buyer with this email or crm_external_id already exists
    get:
      summary: List buyers (admin only)
      tags: [Buyers]
      responses:
        "200":
          description: All buyers
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items:
                      $ref: "#/components/schemas/Buyer"
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
  /api/fb-profiles/oauth/start:
    get:
      summary: Start the Facebook Login flow for a Meta profile (admin only)
      tags: [Meta Profiles]
      description: >
        Returns the Facebook Login dialog URL the admin is redirected to. The
        app runs in Development mode, so only people added to the Meta app as
        Testers/Developers can complete login.
      responses:
        "200":
          description: Facebook Login URL to redirect the admin to
          content:
            application/json:
              schema:
                type: object
                properties:
                  auth_url:
                    type: string
                    example: https://www.facebook.com/v25.0/dialog/oauth?client_id=...
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
  /oauth/facebook/callback:
    get:
      security: []
      summary: Facebook Login redirect target (no auth header; validated by signed state)
      tags: [Meta Profiles]
      description: >
        Facebook redirects the browser here after login. The backend validates
        the signed state, exchanges the authorization code for a long-lived
        (~60 day) token, stores the encrypted profile with its expiry, and
        imports the profile's ad accounts into the tracking pool. Accounts
        already known from another profile are not duplicated.
      parameters:
        - name: code
          in: query
          required: false
          schema:
            type: string
        - name: state
          in: query
          required: false
          schema:
            type: string
        - name: error
          in: query
          required: false
          schema:
            type: string
      responses:
        "200":
          description: Profile connected and ad accounts imported
          content:
            application/json:
              schema:
                type: object
                properties:
                  profile:
                    $ref: "#/components/schemas/FBProfile"
                  ad_accounts:
                    type: array
                    items:
                      $ref: "#/components/schemas/MetaAdAccount"
        "400":
          description: Login denied, missing code, or invalid/expired state
        "502":
          description: Meta token exchange or API request failed
  /api/fb-profiles:
    get:
      summary: List connected Meta profiles (admin only)
      tags: [Meta Profiles]
      responses:
        "200":
          description: All profiles with sync status
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items:
                      $ref: "#/components/schemas/FBProfile"
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
  /api/fb-profiles/{id}/resync:
    post:
      summary: Re-pull the profile's ad accounts and queue a stats sync (admin only)
      tags: [Meta Profiles]
      description: >
        Uses the stored token to refresh the profile's ad-account list (new
        accounts are added to the tracking pool, known ones are updated, nothing
        is duplicated) and enqueues an immediate stats sync.
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: integer
            format: int64
      responses:
        "200":
          description: Ad accounts refreshed
          content:
            application/json:
              schema:
                type: object
                properties:
                  status:
                    type: string
                    example: resynced
                  ad_accounts:
                    type: array
                    items:
                      $ref: "#/components/schemas/MetaAdAccount"
        "404":
          description: Profile not found
        "502":
          description: Meta API request failed (token expired or revoked)
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
  /api/ad-accounts:
    get:
      summary: List ad accounts with current buyer (buyers see only their own)
      tags: [Ad Accounts]
      responses:
        "200":
          description: Ad accounts with current assignment
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items:
                      $ref: "#/components/schemas/AdAccountListItem"
        "401":
          $ref: "#/components/responses/Unauthorized"
  /api/ad-accounts/{id}:
    patch:
      summary: Toggle tracking for ad account (admin only)
      tags: [Ad Accounts]
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
            example: act_123456789
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [is_tracked]
              properties:
                is_tracked:
                  type: boolean
      responses:
        "200":
          description: Tracking flag updated
        "404":
          description: Ad account not found
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
  /api/ad-accounts/{id}/assign:
    post:
      summary: Assign or reassign ad account to buyer (admin only)
      description: Closes the current ownership interval (if any) and opens a new one.
      tags: [Ad Accounts]
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
            example: act_123456789
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [buyer_id]
              properties:
                buyer_id:
                  type: integer
                  format: int64
                  example: 5
      responses:
        "200":
          description: Buyer assigned
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
  /api/ad-accounts/{id}/unassign:
    post:
      summary: Close current buyer assignment (admin only)
      tags: [Ad Accounts]
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
            example: act_123456789
      responses:
        "200":
          description: Assignment closed
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
  /api/ad-accounts/{id}/assignments:
    get:
      summary: Ownership history of an ad account (admin only)
      tags: [Ad Accounts]
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
            example: act_123456789
      responses:
        "200":
          description: Ownership intervals, oldest first
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items:
                      $ref: "#/components/schemas/Assignment"
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
  /api/ad-accounts/{id}/snapshots:
    get:
      summary: Snapshots of one ad account
      description: >
        Cumulative daily-stats snapshots, oldest first. Deltas between
        consecutive snapshots are computed by the frontend. Buyers only receive
        snapshots captured while the account was assigned to them.
      tags: [Snapshots]
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
            example: act_123456789
        - name: from
          in: query
          required: true
          schema:
            type: string
            format: date-time
            example: "2026-06-10T00:00:00Z"
        - name: to
          in: query
          required: true
          schema:
            type: string
            format: date-time
            example: "2026-06-11T00:00:00Z"
      responses:
        "200":
          description: Snapshots in the requested window
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items:
                      $ref: "#/components/schemas/StatSnapshot"
        "400":
          description: Missing or invalid from/to
        "401":
          $ref: "#/components/responses/Unauthorized"
  /api/snapshots:
    get:
      summary: Snapshots across ad accounts
      description: >
        Admins can filter by buyer_id (snapshots captured inside that buyer's
        ownership intervals) and/or ad_account_id. Buyers are always restricted
        to their own ownership intervals.
      tags: [Snapshots]
      parameters:
        - name: from
          in: query
          required: true
          schema:
            type: string
            format: date-time
        - name: to
          in: query
          required: true
          schema:
            type: string
            format: date-time
        - name: buyer_id
          in: query
          required: false
          schema:
            type: integer
            format: int64
        - name: ad_account_id
          in: query
          required: false
          schema:
            type: string
      responses:
        "200":
          description: Snapshots in the requested window
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items:
                      $ref: "#/components/schemas/StatSnapshot"
        "400":
          description: Missing or invalid parameters
        "401":
          $ref: "#/components/responses/Unauthorized"
  /api/alerts:
    get:
      summary: List recent alerts (admin only)
      tags: [Alerts]
      parameters:
        - name: limit
          in: query
          required: false
          schema:
            type: integer
            default: 100
            maximum: 500
      responses:
        "200":
          description: Recent alerts, newest first
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items:
                      $ref: "#/components/schemas/Alert"
        "401":
          $ref: "#/components/responses/Unauthorized"
        "403":
          $ref: "#/components/responses/Forbidden"
  /api/analytics/summary:
    get:
      summary: KPI summary built from interval deltas
      tags: [Analytics]
      parameters:
        - $ref: "#/components/parameters/From"
        - $ref: "#/components/parameters/To"
        - $ref: "#/components/parameters/Timezone"
        - $ref: "#/components/parameters/BuyerID"
        - $ref: "#/components/parameters/AdAccountID"
      responses:
        "200":
          description: KPI cards, account counts, and freshness status
        "401":
          $ref: "#/components/responses/Unauthorized"
  /api/analytics/timeseries:
    get:
      summary: Bucketed KPI time series
      tags: [Analytics]
      parameters:
        - $ref: "#/components/parameters/From"
        - $ref: "#/components/parameters/To"
        - $ref: "#/components/parameters/Timezone"
        - $ref: "#/components/parameters/Granularity"
        - $ref: "#/components/parameters/BuyerID"
        - $ref: "#/components/parameters/AdAccountID"
      responses:
        "200":
          description: Hourly or daily KPI buckets
        "401":
          $ref: "#/components/responses/Unauthorized"
  /api/analytics/ad-accounts:
    get:
      summary: Ad account performance table
      tags: [Analytics]
      parameters:
        - $ref: "#/components/parameters/From"
        - $ref: "#/components/parameters/To"
        - $ref: "#/components/parameters/Timezone"
        - $ref: "#/components/parameters/BuyerID"
        - name: sort
          in: query
          schema:
            type: string
            enum: [spend_desc, roas_desc, cpl_asc]
        - name: limit
          in: query
          schema:
            type: integer
      responses:
        "200":
          description: Account rows with KPIs and freshness
        "401":
          $ref: "#/components/responses/Unauthorized"
  /api/analytics/ad-accounts/{id}:
    get:
      summary: One ad account analytics detail
      tags: [Analytics]
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
        - $ref: "#/components/parameters/From"
        - $ref: "#/components/parameters/To"
        - $ref: "#/components/parameters/Timezone"
        - $ref: "#/components/parameters/Granularity"
      responses:
        "200":
          description: KPI summary, actions, time series, and admin assignment history
        "401":
          $ref: "#/components/responses/Unauthorized"
  /api/analytics/buyers:
    get:
      summary: Buyer performance leaderboard (admin only)
      tags: [Analytics]
      parameters:
        - $ref: "#/components/parameters/From"
        - $ref: "#/components/parameters/To"
        - $ref: "#/components/parameters/Timezone"
      responses:
        "200":
          description: Buyer rows with KPIs
        "403":
          $ref: "#/components/responses/Forbidden"
  /api/analytics/actions:
    get:
      summary: Action type breakdown
      tags: [Analytics]
      parameters:
        - $ref: "#/components/parameters/From"
        - $ref: "#/components/parameters/To"
        - $ref: "#/components/parameters/BuyerID"
        - $ref: "#/components/parameters/AdAccountID"
      responses:
        "200":
          description: Action counts, values, cost per action, and share
  /api/analytics/compare:
    get:
      summary: Compare current and previous ranges
      tags: [Analytics]
      parameters:
        - $ref: "#/components/parameters/From"
        - $ref: "#/components/parameters/To"
        - name: compare_from
          in: query
          required: true
          schema:
            type: string
            format: date-time
        - name: compare_to
          in: query
          required: true
          schema:
            type: string
            format: date-time
      responses:
        "200":
          description: Current, previous, and delta KPI objects
  /api/analytics/pacing:
    get:
      summary: Budget pacing for a period or explicit range
      tags: [Analytics]
      parameters:
        - name: budget
          in: query
          required: true
          schema:
            type: number
        - name: period
          in: query
          schema:
            type: string
            enum: [today, week, month]
        - $ref: "#/components/parameters/Timezone"
      responses:
        "200":
          description: Spend so far, expected spend, projected spend, and pacing status
  /api/analytics/freshness:
    get:
      summary: Data freshness and profile status
      tags: [Analytics]
      parameters:
        - $ref: "#/components/parameters/BuyerID"
      responses:
        "200":
          description: Account freshness and admin profile statuses
  /api/analytics/issues:
    get:
      summary: Actionable data quality and sync issues
      tags: [Analytics]
      responses:
        "200":
          description: Freshness issues and admin alerts
  /api/analytics/export.csv:
    get:
      summary: CSV export of analytics KPIs
      tags: [Analytics]
      parameters:
        - $ref: "#/components/parameters/From"
        - $ref: "#/components/parameters/To"
        - name: group_by
          in: query
          schema:
            type: string
            enum: [ad_account, hour, day]
      responses:
        "200":
          description: CSV file
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
      bearerFormat: JWT
  parameters:
    From:
      name: from
      in: query
      required: true
      schema:
        type: string
        format: date-time
    To:
      name: to
      in: query
      required: true
      schema:
        type: string
        format: date-time
    Timezone:
      name: timezone
      in: query
      required: false
      schema:
        type: string
        example: Asia/Kuala_Lumpur
    Granularity:
      name: granularity
      in: query
      required: false
      schema:
        type: string
        enum: [hour, day]
    BuyerID:
      name: buyer_id
      in: query
      required: false
      schema:
        type: integer
        format: int64
    AdAccountID:
      name: ad_account_id
      in: query
      required: false
      schema:
        type: string
  responses:
    Unauthorized:
      description: Missing or invalid JWT
    Forbidden:
      description: Role is not allowed for this action
  schemas:
    RegisterRequest:
      type: object
      required: [email, password, role]
      properties:
        email:
          type: string
          format: email
          example: admin@example.com
        password:
          type: string
          format: password
          minLength: 8
          example: secret123
        role:
          type: string
          enum: [admin, buyer]
          example: admin
        buyer_id:
          type: integer
          format: int64
          nullable: true
          description: Required when role is buyer.
    LoginRequest:
      type: object
      required: [email, password]
      properties:
        email:
          type: string
          format: email
          example: admin@example.com
        password:
          type: string
          format: password
          example: secret123
    AppUser:
      type: object
      properties:
        id:
          type: integer
          format: int64
        email:
          type: string
          format: email
        role:
          type: string
          enum: [admin, buyer]
        buyer_id:
          type: integer
          format: int64
          nullable: true
        status:
          type: string
          enum: [active, disabled]
        created_at:
          type: string
          format: date-time
        updated_at:
          type: string
          format: date-time
    TokenPair:
      type: object
      properties:
        access_token:
          type: string
        refresh_token:
          type: string
        token_type:
          type: string
          example: Bearer
        access_token_expires_in:
          type: integer
          example: 900
        refresh_token_expires_in:
          type: integer
          example: 2592000
    AuthResponse:
      allOf:
        - $ref: "#/components/schemas/TokenPair"
        - type: object
          properties:
            user:
              $ref: "#/components/schemas/AppUser"
    CreateBuyerRequest:
      type: object
      required: [display_name, email, password]
      properties:
        display_name:
          type: string
          example: Иван Иванов
        email:
          type: string
          format: email
          example: buyer@example.com
        password:
          type: string
          format: password
          minLength: 8
        telegram_username:
          type: string
          nullable: true
          example: ivan_buyer
        crm_external_id:
          type: string
          nullable: true
    Buyer:
      type: object
      properties:
        id:
          type: integer
          format: int64
        crm_external_id:
          type: string
          nullable: true
        display_name:
          type: string
        email:
          type: string
          nullable: true
        telegram_username:
          type: string
          nullable: true
        created_at:
          type: string
          format: date-time
        updated_at:
          type: string
          format: date-time
    FBProfile:
      type: object
      properties:
        id:
          type: integer
          format: int64
        fb_user_id:
          type: string
        fb_name:
          type: string
        status:
          type: string
          enum: [active, checkpoint, expired, disabled]
        status_message:
          type: string
          nullable: true
        token_expires_at:
          type: string
          format: date-time
          nullable: true
          description: When the stored long-lived token expires (~60 days after connect).
    MetaAdAccount:
      type: object
      properties:
        id:
          type: string
          example: act_123456789
        name:
          type: string
        account_status:
          type: integer
          nullable: true
        currency:
          type: string
          nullable: true
        timezone_name:
          type: string
          nullable: true
    AdAccountListItem:
      allOf:
        - $ref: "#/components/schemas/MetaAdAccount"
        - type: object
          properties:
            is_tracked:
              type: boolean
            current_buyer_id:
              type: integer
              format: int64
              nullable: true
            current_buyer_name:
              type: string
              nullable: true
    Assignment:
      type: object
      properties:
        id:
          type: integer
          format: int64
        ad_account_id:
          type: string
        buyer_id:
          type: integer
          format: int64
        buyer_name:
          type: string
        assigned_at:
          type: string
          format: date-time
        unassigned_at:
          type: string
          format: date-time
          nullable: true
          description: Null while the assignment is still active.
    SnapshotMetrics:
      type: object
      description: Cumulative "today so far" metrics at the moment captured_at.
      properties:
        spend:
          type: number
          format: double
        impressions:
          type: integer
          format: int64
        clicks:
          type: integer
          format: int64
        reach:
          type: integer
          format: int64
        frequency:
          type: number
          format: double
        cpc:
          type: number
          format: double
        cpm:
          type: number
          format: double
        ctr:
          type: number
          format: double
    SnapshotAction:
      type: object
      properties:
        action_type:
          type: string
          example: lead
        count:
          type: number
          format: double
        value:
          type: number
          format: double
          description: Monetary value of the conversions (from action_values).
    StatSnapshot:
      type: object
      properties:
        id:
          type: integer
          format: int64
        ad_account_id:
          type: string
        captured_at:
          type: string
          format: date-time
          description: Exact moment the snapshot was taken.
        meta_date:
          type: string
          format: date
          description: The "today" the metrics belong to, in the ad account's timezone.
        metrics:
          $ref: "#/components/schemas/SnapshotMetrics"
        actions:
          type: array
          items:
            $ref: "#/components/schemas/SnapshotAction"
    Alert:
      type: object
      properties:
        id:
          type: integer
          format: int64
        type:
          type: string
          example: TOKEN_INVALID_OR_EXPIRED
        fb_profile_id:
          type: integer
          format: int64
          nullable: true
        ad_account_id:
          type: string
          nullable: true
        buyer_id:
          type: integer
          format: int64
          nullable: true
        message:
          type: string
        payload:
          type: object
        sent_at:
          type: string
          format: date-time
          nullable: true
        created_at:
          type: string
          format: date-time
`
