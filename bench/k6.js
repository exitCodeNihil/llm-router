// The same non-streaming load as `loadgen -rate $RATE -duration 30s`, through k6's
// constant-arrival-rate executor, to cross-check the load generator (./run.sh k6check).
import http from 'k6/http';

export const options = {
  scenarios: {
    load: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE),
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 100,
      maxVUs: 2000,
    },
  },
  summaryTrendStats: ['min', 'med', 'p(90)', 'p(99)', 'max'],
};

export function handleSummary(data) {
  return { [__ENV.OUT]: JSON.stringify(data) };
}

const filler = 'lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor. '.repeat(26).slice(0, 2048);

export default function () {
  const body = JSON.stringify({
    model: __ENV.MODEL,
    max_tokens: 1024,
    messages: [{ role: 'user', content: `LGID:${__VU}${__ITER}: ${filler}` }],
  });
  http.post(__ENV.URL, body, {
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${__ENV.KEY}` },
  });
}
