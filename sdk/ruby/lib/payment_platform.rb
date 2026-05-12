# frozen_string_literal: true
#
# Ruby SDK for Payment Platform.
#
#   require 'payment_platform'
#   pay = PaymentPlatform::Client.new(api_key: 'pk_test_xxx')
#   charge = pay.charges.create(amount: 1999, currency: 'USD', source: 'tok_xxx')

require 'json'
require 'net/http'
require 'openssl'
require 'securerandom'
require 'uri'

module PaymentPlatform
  API_VERSION = '2026-05-01'
  VERSION     = '0.1.0'

  class APIError < StandardError
    attr_reader :status_code, :error_code, :body
    def initialize(message, status_code: nil, error_code: nil, body: nil)
      super(message)
      @status_code = status_code
      @error_code = error_code
      @body = body || {}
    end
  end

  class WebhookSignatureError < StandardError; end

  class Client
    attr_reader :api_key, :base_url, :mode, :timeout, :max_retries

    def initialize(api_key:, base_url: nil, timeout: 30, max_retries: 3)
      raise ArgumentError, 'api_key required' if api_key.nil? || api_key.empty?
      @api_key = api_key
      @mode = api_key.start_with?('pk_test_') ? 'test'
            : api_key.start_with?('pk_live_') ? 'live'
            : 'live'
      @base_url = base_url || (
        @mode == 'test' ? 'https://api-sandbox.payment.example.com'
                        : 'https://api.payment.example.com'
      )
      @timeout = timeout
      @max_retries = max_retries
    end

    def charges;   @charges   ||= Resource.new(self, 'charges'); end
    def refunds;   @refunds   ||= Resource.new(self, 'refunds'); end
    def payouts;   @payouts   ||= Resource.new(self, 'payouts'); end
    def customers; @customers ||= Resource.new(self, 'customers'); end
    def webhooks;  @webhooks  ||= Webhooks.new(self); end

    def request(method, path, body: nil, idempotency_key: nil)
      uri = URI.parse(@base_url + path)
      headers = {
        'Authorization' => "Bearer #{@api_key}",
        'User-Agent'    => "payment_platform-ruby/#{VERSION}",
        'Accept'        => 'application/json',
        'X-API-Version' => API_VERSION,
      }
      data = nil
      if body
        headers['Content-Type'] = 'application/json'
        data = body.to_json
      end
      if %w[POST PUT DELETE PATCH].include?(method.to_s.upcase)
        headers['Idempotency-Key'] = idempotency_key || ('idem_' + SecureRandom.hex(16))
      end

      last_err = nil
      @max_retries.times do |attempt|
        begin
          req_class = case method.to_s.upcase
                      when 'POST'   then Net::HTTP::Post
                      when 'GET'    then Net::HTTP::Get
                      when 'PUT'    then Net::HTTP::Put
                      when 'DELETE' then Net::HTTP::Delete
                      else Net::HTTP::Get
                      end
          req = req_class.new(uri.request_uri, headers)
          req.body = data if data
          http = Net::HTTP.new(uri.host, uri.port)
          http.use_ssl = uri.scheme == 'https'
          http.read_timeout = @timeout
          resp = http.request(req)
          status = resp.code.to_i
          parsed = resp.body && !resp.body.empty? ? JSON.parse(resp.body) : {}

          return parsed if status < 300

          # 4xx (除 429) 不重试
          if status >= 400 && status < 500 && status != 429
            raise APIError.new(parsed['message'] || "HTTP #{status}",
                               status_code: status, error_code: parsed['error'], body: parsed)
          end
          last_err = APIError.new("HTTP #{status}", status_code: status, body: parsed)
        rescue StandardError => e
          raise e if e.is_a?(APIError) && e.status_code && e.status_code < 500 && e.status_code != 429
          last_err = e
        end
        sleep(0.1 * (2**attempt))
      end
      raise last_err || APIError.new('unknown')
    end
  end

  class Resource
    def initialize(client, name)
      @client = client
      @path = "/v1/#{name}"
    end

    def create(idempotency_key: nil, **params)
      @client.request(:post, @path, body: params, idempotency_key: idempotency_key)
    end

    def retrieve(id)
      @client.request(:get, "#{@path}/#{id}")
    end

    def list(**query)
      qs = query.empty? ? '' : ('?' + URI.encode_www_form(query))
      @client.request(:get, @path + qs)
    end
  end

  class Webhooks
    def initialize(client); @client = client; end

    # 同 stripe.Webhook.construct_event.
    #
    #   payload   = request.raw_post   # raw body, 不要先 JSON parse
    #   sig       = request.headers['X-Webhook-Signature']
    #   event     = pay.webhooks.construct_event(payload, sig, ENV['WEBHOOK_SECRET'])
    def construct_event(raw_body, signature_header, endpoint_secret, tolerance: 300)
      timestamp = 0
      sigs = []
      signature_header.split(',').each do |part|
        part = part.strip
        if part.start_with?('t=')
          timestamp = part[2..].to_i
        elsif part.start_with?('v1=')
          sigs << part[3..]
        end
      end
      raise WebhookSignatureError, 'malformed signature header' if timestamp == 0 || sigs.empty?

      now = Time.now.to_i
      if (now - timestamp).abs > tolerance
        raise WebhookSignatureError, "timestamp out of tolerance (delta=#{now - timestamp}s)"
      end

      payload = "#{timestamp}.#{raw_body}"
      expected = OpenSSL::HMAC.hexdigest('SHA256', endpoint_secret, payload)
      unless sigs.any? { |s| secure_compare(s, expected) }
        raise WebhookSignatureError, 'signature mismatch'
      end
      JSON.parse(raw_body)
    end

    private

    def secure_compare(a, b)
      return false if a.bytesize != b.bytesize
      l = a.unpack('C*')
      r = 0
      b.each_byte { |byte| r |= byte ^ l.shift }
      r == 0
    end
  end
end
