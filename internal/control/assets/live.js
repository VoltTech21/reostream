// The camera page's Live tab: one player, the stream picker above it, and
// the quick actions beside it. Also the copy buttons on the Streams
// settings section. Every player is reostreamPlayer (snapshot.js).
(function () {
  'use strict';
  // The prefix the page is served under: "" at the root, or a proxy's or
  // Home Assistant ingress's path. layout.html puts it in a meta tag.
  var pageBase = (document.querySelector('meta[name="reostream-base"]') || {}).content || '';

  function copy(text, done) {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(function () { done(true); }, function () { done(false); });
      return;
    }
    // Plain http on a LAN address has no clipboard API; the old way works.
    var ta = document.createElement('textarea');
    ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta); ta.select();
    var ok = false;
    try { ok = document.execCommand('copy'); } catch (e) {}
    document.body.removeChild(ta);
    done(ok);
  }

  document.querySelectorAll('.copy-btn').forEach(function (btn) {
    btn.addEventListener('click', function () {
      copy(btn.dataset.copy, function (ok) {
        btn.textContent = ok ? 'copied' : 'select it';
        setTimeout(function () { btn.textContent = 'copy'; }, 1200);
      });
    });
  });

  // Pan and tilt: hold an arrow to move, let go to stop. A stop is sent on
  // every way a hold can end, because a move runs until the camera hears one.
  var ptz = document.querySelector('.ptz-card');
  if (ptz) {
    var note = ptz.querySelector('.ptz-note');
    var moving = null;
    // One at a time, in order: each command is its own request, and a stop
    // that overtook its move would leave the camera moving.
    var base = pageBase + '/cameras/' + encodeURIComponent(ptz.dataset.camera) + '/ptz';
    var speed = document.getElementById('ptz-speed');
    var speedOut = ptz.querySelector('.ptz-speed output');
    try {
      var saved = localStorage.getItem('reostream.ptzSpeed');
      if (saved) speed.value = saved;
    } catch (e) {}
    speedOut.textContent = speed.value;
    speed.addEventListener('input', function () {
      speedOut.textContent = speed.value;
      try { localStorage.setItem('reostream.ptzSpeed', speed.value); } catch (e) {}
    });
    var queue = Promise.resolve();
    var post = function (url, params) {
      queue = queue.then(function () {
        return fetch(url, {method: 'POST', body: new URLSearchParams(params)})
          .then(function (r) { if (!r.ok) return r.text().then(function (t) { throw new Error(t); }); note.textContent = ''; })
          .catch(function (e) { note.textContent = 'The camera refused: ' + e.message; throw e; });
      });
      return queue.catch(function () {});
    };
    var send = function (cmd) { post(base, {command: cmd, speed: speed.value}); };

    ptz.querySelectorAll('.ptz-preset').forEach(function (b) {
      b.addEventListener('click', function () { post(base + '/preset', {op: 'goto', id: b.dataset.id}); });
    });
    var save = ptz.querySelector('.ptz-save');
    if (save) {
      save.addEventListener('submit', function (e) {
        e.preventDefault();
        queue = queue.then(function () {
          return fetch(base + '/preset', {method: 'POST', body: new URLSearchParams({op: 'save', id: save.dataset.id, name: save.elements.name.value})})
            .then(function (r) { if (!r.ok) return r.text().then(function (t) { throw new Error(t); }); location.reload(); })
            .catch(function (e) { note.textContent = 'The camera refused: ' + e.message; });
        });
      });
    }
    var stop = function () {
      if (!moving) return;
      moving.classList.remove('held');
      moving = null;
      send('stop');
    };
    ptz.querySelectorAll('.ptz-btn').forEach(function (b) {
      b.addEventListener('pointerdown', function (e) {
        e.preventDefault();
        if (moving) stop();
        moving = b;
        b.classList.add('held');
        try { b.setPointerCapture(e.pointerId); } catch (x) {}
        send(b.dataset.cmd);
      });
      b.addEventListener('pointerup', stop);
      b.addEventListener('pointercancel', stop);
      b.addEventListener('lostpointercapture', stop);
    });
    ptz.querySelector('.ptz-pad').addEventListener('contextmenu', function (e) { e.preventDefault(); });
    window.addEventListener('blur', stop);
    document.addEventListener('visibilitychange', function () { if (document.hidden) stop(); });
  }

  var box = document.querySelector('.live-player');
  if (!box) return;
  var video = box.querySelector('video');
  var tag = box.querySelector('.live-tag');
  var empty = box.querySelector('.live-empty');
  var stats = document.getElementById('live-stats');
  var noteEl = document.getElementById('live-note');
  var snapBtn = document.getElementById('live-snap');
  var fullBtn = document.getElementById('live-full');
  var copyBtn = document.getElementById('live-copy');
  var urls = document.getElementById('live-urls');
  var opts = Array.prototype.slice.call(document.querySelectorAll('.live-streams .seg-opt'));
  var player = null, current = null;
  var key = 'reostream.liveStream.' + box.dataset.camera;

  // H.265 plays where the browser can decode it into MSE (Safari, recent
  // Chrome and Edge with hardware support). Asked, not assumed.
  function canPlay(codec) {
    codec = (codec || '').toUpperCase();
    if (codec === 'H264') return true;
    if (codec !== 'H265' && codec !== 'HEVC') return false;
    var types = ['video/mp4; codecs="hvc1.1.6.L153.B0"', 'video/mp4; codecs="hev1.1.6.L153.B0"', 'video/mp4; codecs="hvc1.1.6.L93.B0"'];
    var ms = [window.MediaSource, window.ManagedMediaSource];
    return ms.some(function (M) { return M && types.some(function (t) { try { return M.isTypeSupported(t); } catch (e) { return false; } }); });
  }
  var cant = [];
  opts.forEach(function (o) {
    var ok = canPlay(o.dataset.codec);
    o.disabled = !ok;
    o.classList.toggle('struck', !ok);
    if (ok) o.removeAttribute('title');
    else {
      o.title = (o.dataset.codec || 'no video yet') + ': this browser can’t play it';
      if (o.dataset.codec) cant.push(o.dataset.stream + ' is ' + (/^H265|HEVC$/i.test(o.dataset.codec) ? 'H.265' : o.dataset.codec));
    }
  });
  if (cant.length) noteEl.textContent = cant.join(', ') + ', which this browser can’t play. Your NVR still gets it.';

  // Pausing disconnects, so play always starts again at live (and a
  // battery camera nobody is watching goes back to sleep). The one pause
  // that is not the viewer's is iPhone Safari's while a confirm() is open
  // (the floodlight asks one): askFirst holds that one off.
  // switching: a pause the page itself caused (stopping the old player
  // when switching stream) arrives after the new player has started, and
  // must not read as the viewer pausing it. Cleared once video plays.
  var userPaused = false, ignorePause = false, switching = false;
  video.addEventListener('playing', function () { switching = false; });
  video.addEventListener('pause', function () {
    if (ignorePause || switching || !player) return;
    userPaused = true;
    stop();
  });
  video.addEventListener('play', function () {
    if (userPaused && current) {
      userPaused = false;
      pick(current);
    }
  });
  function askFirst(question) {
    ignorePause = true;
    var yes = confirm(question);
    setTimeout(function () {
      ignorePause = false;
      if (player && video.paused && !document.hidden) video.play().catch(function () {});
    }, 300);
    return yes;
  }

  // A picture that stops moving while the page is in view gets the stream
  // opened again, since a paused-but-not-ended live feed shows no error.
  //
  // Not for a battery camera: reostream ends its session on purpose to
  // save the battery, and reopening it here would undo that.
  var battery = box.dataset.battery === '1';
  var lastTime = -1, still = 0;
  setInterval(function () {
    if (!player || document.hidden || !current || video.paused) { still = 0; return; }
    if (video.currentTime > 0 && video.currentTime === lastTime) still++; else still = 0;
    lastTime = video.currentTime;
    if (still >= 3) {
      still = 0;
      if (battery) asleep(); else pick(current);
    }
  }, 2000);

  function asleep() {
    stop();
    noteEl.textContent = 'Paused to save battery. Pick a stream to wake it again.';
  }

  function stop() {
    if (!player) return;
    switching = true;
    try { player.pause(); player.unload(); player.detachMediaElement(); player.destroy(); } catch (e) {}
    player = null;
  }

  // The stream's rotate mark, in degrees anticlockwise, applied to the
  // element: a CSS rotation turns clockwise, so it is negated. See the
  // turn-quarter rule in style.css for why a quarter turn swaps the size.
  var turned = 0;
  function turn(deg) {
    turned = ((deg % 360) + 360) % 360;
    box.classList.toggle('turn-quarter', turned === 90 || turned === 270);
    box.classList.toggle('turn-half', turned === 180);
    box.style.setProperty('--turn', (-turned) + 'deg');
  }

  function pick(opt) {
    current = opt;
    if (battery) noteEl.textContent = '';
    opts.forEach(function (o) { o.setAttribute('aria-checked', o === opt ? 'true' : 'false'); });
    stats.textContent = [opt.dataset.codec, opt.dataset.fps + ' fps', opt.dataset.mb + ' Mb'].filter(Boolean).join(' · ');
    try { localStorage.setItem(key, opt.dataset.stream); } catch (e) {}
    stop();
    turn(+opt.dataset.rotate || 0);
    if (!opt.dataset.url || opt.disabled || !window.mpegts || !mpegts.isSupported()) return;
    player = reostreamPlayer(opt.dataset.url, video);
    player.attachMediaElement(video);
    player.load();
    reostreamAudio.play(video);
    video.hidden = false;
    tag.hidden = false;
    empty.hidden = true;
    snapBtn.disabled = false;
  }

  opts.forEach(function (o) {
    o.addEventListener('click', function () { if (!o.disabled) pick(o); });
  });
  // The stream last chosen for this camera, else the sharpest this browser
  // plays: main, then extern, then sub.
  var saved = null;
  try { saved = localStorage.getItem(key); } catch (e) {}
  var byName = function (n) { return opts.filter(function (o) { return o.dataset.stream === n && !o.disabled; })[0]; };
  // A battery camera starts on sub: smaller, quicker to wake, and H.264.
  var first = battery ? (byName(saved) || byName('sub') || byName('main'))
    : (byName(saved) || byName('main') || byName('extern') || byName('sub'));
  if (first) pick(first);
  else if (opts.length) { empty.textContent = 'No stream this browser can play.'; empty.hidden = false; }

  // Fullscreen: the player box, so the LIVE tag stays; iPhone Safari only
  // lets the video element itself go full screen.
  fullBtn.addEventListener('click', function () {
    if (box.requestFullscreen) box.requestFullscreen().catch(function () {});
    else if (box.webkitRequestFullscreen) box.webkitRequestFullscreen();
    else if (video.webkitEnterFullscreen) video.webkitEnterFullscreen();
  });

  // Snapshot: the frame on screen, saved as a PNG. Nothing goes to the camera.
  snapBtn.addEventListener('click', function () {
    if (!video.videoWidth) return;
    // Saved the way it is shown: turned like the player when the stream
    // is marked to be.
    var c = document.createElement('canvas');
    var w = video.videoWidth, h = video.videoHeight, quarter = turned === 90 || turned === 270;
    c.width = quarter ? h : w; c.height = quarter ? w : h;
    var g = c.getContext('2d');
    g.translate(c.width / 2, c.height / 2);
    g.rotate(-turned * Math.PI / 180);
    g.drawImage(video, -w / 2, -h / 2);
    var a = document.createElement('a');
    var d = new Date(), pad = function (n) { return (n < 10 ? '0' : '') + n; };
    a.download = box.dataset.camera + '-' + d.getFullYear() + pad(d.getMonth() + 1) + pad(d.getDate()) + '-' +
      pad(d.getHours()) + pad(d.getMinutes()) + pad(d.getSeconds()) + '.png';
    a.href = c.toDataURL('image/png');
    a.click();
  });

  // Copy URL: every address for this camera, pick one.
  copyBtn.addEventListener('click', function () {
    urls.hidden = !urls.hidden;
    copyBtn.setAttribute('aria-expanded', urls.hidden ? 'false' : 'true');
  });
  urls.querySelectorAll('.live-url').forEach(function (b) {
    b.addEventListener('click', function () {
      var done = b.querySelector('.live-url-done');
      copy(b.dataset.copy, function (ok) {
        done.textContent = ok ? 'copied' : 'copy failed';
        setTimeout(function () { done.textContent = 'copy'; }, 1400);
      });
    });
  });

  // Floodlight: on, or back to what it was doing before (off, motion or
  // schedule).
  var flood = document.getElementById('live-flood');
  if (flood) {
    flood.addEventListener('click', function () {
      var on = flood.getAttribute('aria-pressed') === 'true';
      var to = on ? (flood.dataset.was || 'off') : 'on';
      if (!askFirst(on ? 'Turn the floodlight ' + (to === 'off' ? 'off' : 'back to ' + to) + '?' : 'Turn the floodlight on?')) return;
      flood.disabled = true;
      fetch(pageBase + '/cameras/' + encodeURIComponent(flood.dataset.camera) + '/floodlight', {method: 'POST', body: new URLSearchParams({option: to}), credentials: 'same-origin'})
        .then(function (r) { return r.text().then(function (html) { return {ok: r.ok, html: html}; }); })
        .then(function (res) {
          var f = new DOMParser().parseFromString(res.html, 'text/html').querySelector('.flash');
          if (!res.ok || (f && f.classList.contains('flash-refused'))) throw new Error(f ? f.textContent.trim() : 'refused');
          flood.setAttribute('aria-pressed', on ? 'false' : 'true');
        })
        .catch(function (e) { alertNote('Floodlight: ' + e.message); })
        .then(function () { flood.disabled = false; });
    });
  }
  function alertNote(t) {
    var n = document.getElementById('live-msg');
    if (n) n.textContent = t;
  }

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) stop(); else if (current) pick(current);
  });
})();
