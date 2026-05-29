// k6 performance test for the auction pipeline
// Run: k6 run tests/k6/auction.js
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';

const auctionLatency = new Trend('auction_latency_ms');
const winRate = new Rate('win_rate');

export const options = {
    scenarios: {
        steady: {
            executor: 'constant-arrival-rate',
            rate: 10,
            timeUnit: '1s',
            duration: '1m',
            preAllocatedVUs: 20,
        },
        burst: {
            executor: 'ramping-arrival-rate',
            startRate: 10,
            timeUnit: '1s',
            stages: [
                { duration: '30s', target: 100 },
                { duration: '30s', target: 100 },
                { duration: '30s', target: 10 },
            ],
            preAllocatedVUs: 200,
            startTime: '2m',
        },
    },
    thresholds: {
        'auction_latency_ms': ['p(95)<200', 'p(99)<500'],
        'win_rate': ['rate>0.3'],
        'http_req_failed': ['rate<0.01'],
    },
};

const EXCHANGE_URL = __ENV.EXCHANGE_URL || 'http://localhost:8081';
const geos = ['GBR', 'USA', 'DEU', 'FRA', 'JPN'];
const devices = [1, 2, 5]; // mobile, desktop, tablet

export default function () {
    const geo = geos[Math.floor(Math.random() * geos.length)];
    const device = devices[Math.floor(Math.random() * devices.length)];
    const floor = 0.5 + Math.random() * 2.0;

    const payload = JSON.stringify({
        id: `k6-${Date.now()}-${Math.random().toString(36).substr(2, 6)}`,
        imp: [{ id: 'imp-1', banner: { w: 300, h: 250 }, bidfloor: floor }],
        site: { domain: 'k6-test.com' },
        device: { devicetype: device, geo: { country: geo } },
        tmax: 100,
    });

    const res = http.post(`${EXCHANGE_URL}/v1/openrtb/auction`, payload, {
        headers: { 'Content-Type': 'application/json' },
    });

    check(res, {
        'status is 200': (r) => r.status === 200,
        'has valid JSON': (r) => {
            try { JSON.parse(r.body); return true; } catch { return false; }
        },
    });

    auctionLatency.add(res.timings.duration);

    const body = JSON.parse(res.body);
    winRate.add(body.seatbid && body.seatbid.length > 0);

    sleep(0.01);
}
