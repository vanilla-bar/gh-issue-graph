const test = require('node:test')
const assert = require('node:assert/strict')
const timing = require('../web/refresh.js')

test('retryDelay backs off exponentially and stops at the cap', () => {
  assert.equal(timing.retryDelay(1), timing.retryBaseDelay)
  assert.equal(timing.retryDelay(2), timing.retryBaseDelay * 2)
  assert.equal(timing.retryDelay(3), timing.retryBaseDelay * 4)
  assert.equal(timing.retryDelay(99), timing.retryMaxDelay)
})

test('nextDelay waits out the remainder of the refresh interval', () => {
  const now = 1_000_000
  const lastUpdated = now - 60_000
  assert.equal(timing.nextDelay(now, lastUpdated, 0, 0), timing.refreshInterval - 60_000)
})

test('nextDelay never returns less than the minimum', () => {
  const now = 1_000_000
  assert.equal(timing.nextDelay(now, now - timing.refreshInterval * 2, 0, 0), timing.minimumDelay)
})

test('nextDelay follows the backoff schedule while failing', () => {
  const now = 1_000_000
  assert.equal(timing.nextDelay(now, now, 3, now + 45_000), 45_000)
})
