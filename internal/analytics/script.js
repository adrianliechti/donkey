// donkey web analytics. donkey-gateway adds this script to the HTML pages of
// web apps; it stores nothing in the browser and sends nothing but what is
// below. Exclude a browser with localStorage.setItem('donkey:ignore', '1').
(function () {
  'use strict'
  var w = window, d = document, n = navigator, l = location
  if (w.donkey && w.donkey.track) return

  var endpoint = '/_donkey/e.json'
  var off = !!n.webdriver
  try { off = off || w.localStorage.getItem('donkey:ignore') === '1' } catch (e) {}
  var zone = ''
  try { zone = Intl.DateTimeFormat().resolvedOptions().timeZone || '' } catch (e) {}

  function send(body) {
    if (off) return
    body.u = body.u || l.href
    body.l = n.language || ''
    body.z = zone
    body.w = (w.screen && w.screen.width) || 0
    var data = JSON.stringify(body)
    try { if (n.sendBeacon && n.sendBeacon(endpoint, data)) return } catch (e) {}
    try { fetch(endpoint, { method: 'POST', body: data, keepalive: true, credentials: 'same-origin' }) } catch (e) {}
  }

  // A page is its path, plus the route of a hash router (#/…). Query
  // strings and other fragments never make a new page.
  function page() {
    var hash = /^#!?\//.test(l.hash) ? l.hash.split('?')[0] : ''
    return l.pathname + hash
  }

  var current = null, href = '', first = true
  var visible = 0, since = 0, hiddenAt = 0

  function start() { if (!since) since = Date.now() }
  function stop() { if (since) { visible += Date.now() - since; since = 0 } }

  // How long the page stayed visible, sent when it is left or hidden.
  function engagement() {
    stop()
    if (current && visible >= 1000) send({ k: 'engagement', u: href, e: visible })
    visible = 0
  }

  function view() {
    if (page() === current) return
    if (current) engagement()
    current = page()
    href = l.href
    send({ k: 'pageview', r: first ? d.referrer : '' })
    first = false
    if (d.visibilityState === 'visible') start()
  }

  function track(name, props) {
    if (typeof name !== 'string' || !name) return
    var data = {}
    if (props && typeof props === 'object') {
      for (var key in props) {
        if (Object.prototype.hasOwnProperty.call(props, key) && props[key] != null) data[key] = String(props[key])
      }
    }
    send({ k: 'event', n: name, d: data })
  }

  function click(ev) {
    if (ev.type === 'auxclick' && ev.button !== 1) return
    var el = ev.target && ev.target.closest && ev.target.closest('[data-donkey-event],a[href]')
    if (!el) return
    var name = el.getAttribute('data-donkey-event')
    if (name) {
      var props = {}
      for (var i = 0; i < el.attributes.length; i++) {
        var attr = el.attributes[i]
        if (attr.name.indexOf('data-donkey-event-') === 0) props[attr.name.slice(18)] = attr.value
      }
      track(name, props)
      return
    }
    try {
      var url = new URL(el.href, l.href)
      if (/^https?:$/.test(url.protocol) && url.host !== l.host) track('outbound-link', { url: url.hostname + url.pathname })
    } catch (e) {}
  }

  var push = history.pushState, replace = history.replaceState
  history.pushState = function () { var r = push.apply(this, arguments); view(); return r }
  history.replaceState = function () { var r = replace.apply(this, arguments); view(); return r }
  w.addEventListener('popstate', view)
  w.addEventListener('hashchange', view)
  d.addEventListener('click', click, true)
  d.addEventListener('auxclick', click, true)
  d.addEventListener('visibilitychange', function () {
    if (d.visibilityState === 'hidden') {
      engagement()
      hiddenAt = Date.now()
      return
    }
    // Back after the 30 minutes that end a visit: this is a new one.
    if (hiddenAt && Date.now() - hiddenAt > 30 * 60 * 1000) { current = null; view() } else start()
    hiddenAt = 0
  })
  w.addEventListener('pagehide', engagement)
  w.addEventListener('pageshow', function (ev) { if (ev.persisted) { current = null; view() } })

  w.donkey = w.donkey || {}
  w.donkey.track = track
  if (d.prerendering) d.addEventListener('prerenderingchange', view, { once: true })
  else view()
})()
