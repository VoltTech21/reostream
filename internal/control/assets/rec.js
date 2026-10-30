// Recordings: play a clip off the camera's card. The camera sends the
// range as it reads it, so a clip starts within a few seconds and plays as
// it arrives.
(function () {
  'use strict';
  // The prefix the page is served under: "" at the root, or a proxy's or
  // Home Assistant ingress's path. layout.html puts it in a meta tag.
  var pageBase = (document.querySelector('meta[name="reostream-base"]') || {}).content || '';
  var box = document.getElementById('rec-player');
  if (!box) return;
  var video = box.querySelector('video'), empty = document.getElementById('rec-empty');
  var now = document.getElementById('rec-now'), dl = document.getElementById('rec-dl');
  var clips = Array.prototype.slice.call(document.querySelectorAll('.rec-clip'));
  var spans = document.querySelectorAll('.rec-span');
  var base = pageBase + '/cameras/' + encodeURIComponent(box.dataset.camera) + '/recordings/clip';
  var stream = 'sub', cur = -1, player = null;
  try { stream = localStorage.getItem('reostream.recStream') || 'sub'; } catch (e) {}

  function stop() {
    if (!player) return;
    try { player.pause(); player.unload(); player.detachMediaElement(); player.destroy(); } catch (e) {}
    player = null;
  }
  function url(c, download) {
    if (c.dataset.saved) return base.replace(/\/clip$/, '/saved') + '?f=' + encodeURIComponent(c.dataset.saved) + (download ? '&download=1' : '');
    return base + '?start=' + c.dataset.start + '&end=' + c.dataset.end + '&stream=' + stream +
      '&audio=' + c.dataset.audio + (download ? '&download=1' : '');
  }
  function play(i) {
    if (i < 0 || i >= clips.length) return;
    cur = i;
    var c = clips[i];
    clips.forEach(function (x, j) { x.setAttribute('aria-current', j === i ? 'true' : 'false'); });
    spans.forEach(function (x) { x.classList.toggle('current', +x.dataset.i === i); });
    c.scrollIntoView({block: 'nearest'});
    now.textContent = c.dataset.start.replace('T', ' ') + ' → ' + c.dataset.end.slice(11);
    dl.href = url(c, true); dl.hidden = false;
    stop();
    if (c.dataset.saved) {
      // Saved here: a plain file the browser plays and seeks on its own.
      video.src = url(c, false);
      video.hidden = false;
      empty.hidden = true;
      if (window.reostreamAudio) reostreamAudio.play(video); else video.play().catch(function () {});
      return;
    }
    video.removeAttribute('src');
    empty.textContent = 'Loading from the camera…'; empty.hidden = false;
    if (!window.mpegts || !mpegts.isSupported()) { empty.textContent = 'This browser cannot play the clip; use Download.'; return; }
    player = mpegts.createPlayer({type: 'mpegts', isLive: false, url: url(c, false)}, {enableStashBuffer: true});
    player.attachMediaElement(video);
    player.load();
    video.hidden = false;
    video.addEventListener('playing', function once() { video.removeEventListener('playing', once); empty.hidden = true; });
    if (window.reostreamAudio) reostreamAudio.play(video); else video.play().catch(function () {});
  }
  clips.forEach(function (c, i) { c.addEventListener('click', function () { play(i); }); });
  spans.forEach(function (s) { s.addEventListener('click', function () { play(+s.dataset.i); }); });
  function step(d) {
    for (var i = cur + d; i >= 0 && i < clips.length; i += d) {
      if (!clips[i].parentNode.hidden) { play(i); return; }
    }
  }
  document.getElementById('rec-prev').addEventListener('click', function () { step(-1); });
  document.getElementById('rec-next').addEventListener('click', function () { step(1); });
  video.addEventListener('ended', function () { step(1); });

  document.querySelectorAll('.rec-stream .seg-opt').forEach(function (b) {
    b.setAttribute('aria-checked', b.dataset.stream === stream ? 'true' : 'false');
    b.addEventListener('click', function () {
      stream = b.dataset.stream;
      try { localStorage.setItem('reostream.recStream', stream); } catch (e) {}
      document.querySelectorAll('.rec-stream .seg-opt').forEach(function (o) { o.setAttribute('aria-checked', o === b ? 'true' : 'false'); });
      if (cur >= 0) play(cur);
    });
  });
  // Type filters hide clips from the list and the bar.
  document.querySelectorAll('.kind-chip').forEach(function (chip) {
    chip.addEventListener('click', function () {
      var on = chip.getAttribute('aria-pressed') !== 'true';
      chip.setAttribute('aria-pressed', on ? 'true' : 'false');
      clips.forEach(function (c) { if (c.dataset.kind === chip.dataset.kind) c.parentNode.hidden = !on; });
      spans.forEach(function (s) { if (clips[+s.dataset.i].dataset.kind === chip.dataset.kind) s.hidden = !on; });
    });
  });
  document.addEventListener('keydown', function (e) {
    var t = e.target && e.target.tagName;
    if (t === 'INPUT' || t === 'TEXTAREA') return;
    if (e.key === 'ArrowRight' && e.altKey) step(1);
    if (e.key === 'ArrowLeft' && e.altKey) step(-1);
  });
})();
