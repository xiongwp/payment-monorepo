# Cloudflare CDN — 前端静态资源 (admin web, checkout SDK, trace viewer)。
#
# 部署:
#   cd deploy/cdn
#   terraform init
#   terraform apply -var "cf_token=$CF_TOKEN" -var "zone_id=$ZONE_ID"

variable "cf_token"  { sensitive = true }
variable "zone_id"   {}
variable "domain"    { default = "static.payment.example.com" }
variable "origin"    { default = "static-origin.payment.example.com" }

terraform {
  required_providers {
    cloudflare = { source = "cloudflare/cloudflare", version = "~> 4.0" }
  }
}

provider "cloudflare" {
  api_token = var.cf_token
}

# ─── Page Rules (cache strategy) ────────────────────────────────────
# /static/js/*.[hash].js  →  cache_everything + edge_ttl=1y (immutable + hash-busting)
resource "cloudflare_page_rule" "static_immutable" {
  zone_id  = var.zone_id
  target   = "${var.domain}/static/*.*.js"
  priority = 1
  actions {
    cache_level  = "cache_everything"
    edge_cache_ttl = 31536000   # 1 year
    browser_cache_ttl = 31536000
  }
}

# /static/css/*.[hash].css  →  同
resource "cloudflare_page_rule" "static_css" {
  zone_id  = var.zone_id
  target   = "${var.domain}/static/*.*.css"
  priority = 2
  actions {
    cache_level  = "cache_everything"
    edge_cache_ttl = 31536000
    browser_cache_ttl = 31536000
  }
}

# index.html, manifest.json → 短 cache (才能 invalidate 快)
resource "cloudflare_page_rule" "html_short_cache" {
  zone_id  = var.zone_id
  target   = "${var.domain}/*.html"
  priority = 3
  actions {
    cache_level  = "cache_everything"
    edge_cache_ttl = 300        # 5min
    browser_cache_ttl = 60
  }
}

# /api/* 完全不缓存
resource "cloudflare_page_rule" "api_no_cache" {
  zone_id  = var.zone_id
  target   = "${var.domain}/api/*"
  priority = 4
  actions {
    cache_level = "bypass"
  }
}

# ─── DNS ──────────────────────────────────────────────────────────
resource "cloudflare_record" "static" {
  zone_id = var.zone_id
  name    = "static"
  value   = var.origin
  type    = "CNAME"
  proxied = true
  ttl     = 1
}

# ─── WAF custom rules ─────────────────────────────────────────────
resource "cloudflare_ruleset" "waf_block_known_bad" {
  zone_id = var.zone_id
  name    = "block-known-bad-bots"
  kind    = "zone"
  phase   = "http_request_firewall_custom"

  rules {
    action  = "block"
    expression = "(cf.client.bot) or (cf.threat_score gt 30)"
    description = "Block bots and high threat-score requests"
  }
}

# ─── Rate Limiting ────────────────────────────────────────────────
resource "cloudflare_rate_limit" "global" {
  zone_id  = var.zone_id
  threshold = 1000
  period    = 60
  action {
    mode    = "challenge"
    timeout = 60
  }
  match {
    request {
      url_pattern = "${var.domain}/*"
      schemes     = ["HTTPS"]
      methods     = ["GET", "POST"]
    }
  }
}

output "cdn_domain" { value = var.domain }
output "page_rules" {
  value = [
    cloudflare_page_rule.static_immutable.target,
    cloudflare_page_rule.html_short_cache.target,
    cloudflare_page_rule.api_no_cache.target,
  ]
}
