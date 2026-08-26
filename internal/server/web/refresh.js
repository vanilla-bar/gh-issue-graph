// Pure scheduling helpers, kept apart from the DOM so they can be tested with
// plain `node --test`.
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory()
  } else {
    root.refreshTiming = factory()
  }
})(typeof self !== 'undefined' ? self : this, function () {
  const refreshInterval = 300000 // 5 minutes
  const retryBaseDelay = 30000
  const retryMaxDelay = 300000
  const minimumDelay = 1000

  // Exponential backoff, capped, so a failing token does not hammer the API.
  function retryDelay(failures) {
    return Math.min(retryMaxDelay, retryBaseDelay * 2 ** Math.max(0, failures - 1))
  }

  // Time until the next load: the remainder of the refresh interval when
  // healthy, the remainder of the backoff when not.
  function nextDelay(now, lastUpdated, failures, retryAt) {
    if (failures > 0) return Math.max(minimumDelay, retryAt - now)
    return Math.max(minimumDelay, refreshInterval - (now - lastUpdated))
  }

  return { refreshInterval, retryBaseDelay, retryMaxDelay, minimumDelay, retryDelay, nextDelay }
})
