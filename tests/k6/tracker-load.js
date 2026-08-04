// k6 performance test for the tracker's pixel-ingest hot path.
//
// Run: make perf-tracker             (or: k6 run tests/k6/tracker-load.js)
// Tweak: RPS=200 DURATION=2m make perf-tracker
//
// Fires impression + click beacons with realistic params (32-hex trace ids,
// uuid entity ids, CPM price) straight at the tracker. Beacon signature
// enforcement is off by default locally, so unsigned pixels are accepted; the
// tracker's job here is ingest latency + error rate under volume, not
// attribution correctness (the e2e suite owns that). Expect 200/204s; a 4xx
// flood means signature enforcement is on (security harness) — run
// `make security-harness ARG=off` first.
import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';

const beaconLatency = new Trend('beacon_latency_ms');

const RPS = Number(__ENV.RPS || 50);
const DURATION = __ENV.DURATION || '1m';
const TRACKER_URL = __ENV.TRACKER_URL || 'http://localhost:8083';

export const options = {
    scenarios: {
        steady: {
            executor: 'constant-arrival-rate',
            rate: RPS,
            timeUnit: '1s',
            duration: DURATION,
            preAllocatedVUs: Math.max(20, RPS * 2),
        },
    },
    thresholds: {
        'beacon_latency_ms': ['p(95)<100', 'p(99)<250'],
        'http_req_failed': ['rate<0.01'],
    },
};

function hex(n) {
    let s = '';
    for (let i = 0; i < n; i++) s += Math.floor(Math.random() * 16).toString(16);
    return s;
}

function uuid() {
    return `${hex(8)}-${hex(4)}-${hex(4)}-${hex(4)}-${hex(12)}`;
}

export default function () {
    const tid = hex(32); // OTel-shaped trace id
    const common =
        `tid=${tid}&cid=${uuid()}&crid=${uuid()}&pid=${uuid()}` +
        `&pubid=${uuid()}&price=${(1 + Math.random() * 6).toFixed(4)}&cur=USD`;

    const imp = http.get(`${TRACKER_URL}/v1/t/imp?${common}`);
    beaconLatency.add(imp.timings.duration);
    check(imp, { 'imp accepted': (r) => r.status >= 200 && r.status < 300 });

    // ~5% of impressions click through, mirroring the simulator's click rate.
    // The click endpoint is a REDIRECT handler — it requires a redir= target
    // and answers 302; redirects:0 stops k6 following to the (fake) landing
    // page, and 3xx counts as expected via responseCallback below.
    if (Math.random() < 0.05) {
        const clk = http.get(
            `${TRACKER_URL}/v1/t/click?${common}&redir=${encodeURIComponent('https://advertiser.example/landing')}`,
            { redirects: 0, responseCallback: http.expectedStatuses({ min: 200, max: 399 }) },
        );
        beaconLatency.add(clk.timings.duration);
        check(clk, { 'click accepted': (r) => r.status >= 200 && r.status < 400 });
    }
}
