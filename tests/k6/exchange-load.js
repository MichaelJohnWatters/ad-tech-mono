// k6 performance test for the auction pipeline (exchange direct, OpenRTB HTTP).
//
// Run: make perf-exchange            (or: k6 run tests/k6/exchange-load.js)
// Tweak: RPS=100 DURATION=5m make perf-exchange
//
// Requests mirror the simulator's --direct mode so they can actually WIN under
// the exchange's default policies: a structurally-valid IAB SupplyChain rides
// source.ext.schain (schain enforcement is STRICT by default — a bare request
// is rejected with nbr and the old version of this script measured nothing but
// rejections), and imp.tagid + site.publisher.id reference REAL seeded
// placements so DSP targeting can match. The Makefile target queries Postgres
// for live placement/publisher pairs and passes them via PLACEMENTS; without
// it the script still runs but win_rate will be ~0 (rejection-latency only)
// and the win_rate threshold is skipped.
//
// PLACEMENTS format: "tagid|publisherID|domain,tagid|publisherID|domain,..."
import http from 'k6/http';
import { check } from 'k6';
import { Rate, Trend } from 'k6/metrics';

const auctionLatency = new Trend('auction_latency_ms');
const winRate = new Rate('win_rate');

const RPS = Number(__ENV.RPS || 10);
const DURATION = __ENV.DURATION || '1m';
const BURST_TARGET = Number(__ENV.BURST_RPS || RPS * 10);

const placements = (__ENV.PLACEMENTS || '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => {
        const [tagid, pub, domain] = s.split('|');
        return { tagid, pub, domain: domain || 'k6-test.com' };
    });

export const options = {
    scenarios: {
        steady: {
            executor: 'constant-arrival-rate',
            rate: RPS,
            timeUnit: '1s',
            duration: DURATION,
            preAllocatedVUs: Math.max(20, RPS * 2),
        },
        burst: {
            executor: 'ramping-arrival-rate',
            startRate: RPS,
            timeUnit: '1s',
            stages: [
                { duration: '30s', target: BURST_TARGET },
                { duration: '30s', target: BURST_TARGET },
                { duration: '30s', target: RPS },
            ],
            preAllocatedVUs: Math.max(200, BURST_TARGET * 2),
            startTime: DURATION,
        },
    },
    thresholds: Object.assign(
        {
            'auction_latency_ms': ['p(95)<200', 'p(99)<500'],
            'http_req_failed': ['rate<0.01'],
        },
        // win_rate is only meaningful when real placements are wired in.
        placements.length > 0 ? { 'win_rate': ['rate>0.3'] } : {},
    ),
};

const EXCHANGE_URL = __ENV.EXCHANGE_URL || 'http://localhost:8081';
const geos = ['GBR', 'USA', 'DEU', 'FRA', 'JPN'];
const devices = [1, 2, 5]; // mobile, desktop, tablet

export default function () {
    const geo = geos[Math.floor(Math.random() * geos.length)];
    const device = devices[Math.floor(Math.random() * devices.length)];
    const floor = 0.5 + Math.random() * 2.0;
    const reqID = `k6-${Date.now()}-${Math.random().toString(36).substr(2, 6)}`;
    const pl = placements.length
        ? placements[Math.floor(Math.random() * placements.length)]
        : null;

    const payload = JSON.stringify({
        id: reqID,
        imp: [{
            id: 'imp-1',
            tagid: pl ? pl.tagid : undefined,
            banner: { w: 300, h: 250 },
            bidfloor: floor,
        }],
        site: {
            domain: pl ? pl.domain : 'k6-test.com',
            publisher: pl ? { id: pl.pub } : undefined,
        },
        device: { devicetype: device, geo: { country: geo } },
        // Structurally-valid supply chain (same stand-in seller the simulator
        // uses) — required to pass the exchange's strict schain gate.
        source: {
            ext: {
                schain: {
                    ver: '1.0',
                    complete: 1,
                    nodes: [{ asi: 'sim-ssp.adtech.local', sid: 'sim-seller-01', rid: reqID, hp: 1 }],
                },
            },
        },
        tmax: 100,
        cur: ['USD'],
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

    try {
        const body = JSON.parse(res.body);
        winRate.add(!!(body.seatbid && body.seatbid.length > 0));
    } catch (_) {
        winRate.add(false);
    }
}
