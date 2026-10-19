// k6 smoke profile of the merchant invoice hot path (CBL-17.14, section 10.5):
// a mixed read/mutation load against the global stand. The full 3000 RPS
// profile is task 14.1 (KI-billing-api-05). The create share is deliberately
// low: the connector enforces the provider manifest budget (BTCPay: 100
// invoices/hour), so a mutation-heavy run drains the stand bucket and answers
// 503 by design; the reads carry the volume.
//
// Usage:
//   docker run --rm -i --network host -e BASE_URL=http://localhost:8010 \
//     -e TOKEN=<merchant key> grafana/k6 run - < test/load/invoices-smoke.js
import http from 'k6/http';
import { check } from 'k6';
import { randomString } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const BASE = __ENV.BASE_URL || 'http://localhost:8010';
const TOKEN = __ENV.TOKEN || '';

export const options = {
  vus: 10,
  duration: '15s',
  thresholds: {
    http_req_failed: ['rate<0.01'],
    'http_req_duration{kind:create}': ['p(95)<800'],
    'http_req_duration{kind:read}': ['p(95)<300'],
  },
};

const headers = {
  Authorization: `Bearer ${TOKEN}`,
  'Content-Type': 'application/json',
};

export default function () {
  if (Math.random() < 0.003) {
    const body = JSON.stringify({
      merchant_order_id: `smoke-${randomString(12)}`,
      amount: '0.001',
      currency: 'BTC',
      pay_currency: 'USDT',
      network: 'trc20',
    });
    const response = http.post(`${BASE}/v1/invoices`, body, {
      headers: { ...headers, 'Idempotency-Key': `smoke-${randomString(16)}` },
      tags: { kind: 'create' },
    });
    check(response, { 'create accepted': (r) => r.status === 201 });
    return;
  }
  const path = Math.random() < 0.5 ? '/v1/invoices?limit=5' : '/v1/balances';
  const response = http.get(`${BASE}${path}`, { headers, tags: { kind: 'read' } });
  check(response, { 'read served': (r) => r.status === 200 });
}
