// fisheyeview.js: wires the dewarp page up -- stream in, panes out, drag to
// look around. The projection itself lives in fisheye.js; this file is only
// plumbing and controls.

(function () {
  'use strict';

  var root = document.getElementById('dewarp');
  if (!root) return;

  var canvas = document.getElementById('dewarp-canvas');
  var video = document.getElementById('dewarp-video');
  var note = document.getElementById('dewarp-note');
  var url = root.getAttribute('data-url');

  function fail(msg) {
    if (note) note.textContent = msg;
  }

  var r;
  try {
    r = new window.ReoFisheye.Renderer(canvas, video);
  } catch (e) {
    fail(e.message + '. The raw circle is still available on the camera page.');
    return;
  }

  // The stream. Same origin as this page by construction -- see
  // control.go's note on why the tiles mount exists at all.
  var player = null;
  function start() {
    if (player || !window.mpegts || !mpegts.isSupported()) {
      if (!window.mpegts || !mpegts.isSupported()) fail('This browser cannot play MPEG-TS.');
      return;
    }
    player = reostreamPlayer(url);
    player.attachMediaElement(video);
    player.load();
    var p = video.play();
    if (p && p.catch) p.catch(function () { /* autoplay refusal is not fatal */ });
  }

  function stop() {
    if (!player) return;
    // Detaching is not enough: mpegts.js keeps pulling over XHR until it is
    // destroyed, which is the whole reason the dashboard tears players down
    // rather than pausing them.
    try { player.destroy(); } catch (e) { /* already gone */ }
    player = null;
  }

  // Draw loop. requestAnimationFrame stops on a hidden tab by itself, so a
  // backgrounded page costs nothing.
  var running = true;
  function frame() {
    if (!running) return;
    try { r.draw(); } catch (e) { /* a lost context recovers on the next frame */ }
    requestAnimationFrame(frame);
  }
  requestAnimationFrame(frame);

  window.addEventListener('pagehide', function () { running = false; stop(); });

  // A hidden tab stops streaming, and starts again when it comes back.
  //
  // pagehide alone is not enough: it fires on navigation and close, not on
  // a tab moved to the background, so a page left open on a second monitor
  // keeps pulling a multi-megabit live stream into a MediaSource buffer
  // indefinitely. Measured the hard way -- a tab left open for nine hours
  // during development reached 6.5 GB. requestAnimationFrame already pauses
  // itself when hidden; the stream does not, so it is stopped here.
  document.addEventListener('visibilitychange', function () {
    if (document.hidden) {
      stop();
    } else if (running) {
      start();
    }
  });

  // Dragging aims the pane under the pointer. Sensitivity is tied to the
  // pane's zoom so a tight view does not fly off when nudged.
  var dragging = null, lastX = 0, lastY = 0;

  function localPoint(ev) {
    var box = canvas.getBoundingClientRect();
    return { x: ev.clientX - box.left, y: ev.clientY - box.top };
  }

  canvas.addEventListener('pointerdown', function (ev) {
    var p = localPoint(ev);
    dragging = r.paneAt(p.x, p.y);
    lastX = ev.clientX;
    lastY = ev.clientY;
    canvas.setPointerCapture(ev.pointerId);
    canvas.classList.add('grabbing');
  });

  canvas.addEventListener('pointermove', function (ev) {
    if (!dragging) return;
    var k = dragging.zoom / canvas.clientHeight * 2.2;
    dragging.aim(dragging.yaw - (ev.clientX - lastX) * k,
                 dragging.pitch - (ev.clientY - lastY) * k);
    lastX = ev.clientX;
    lastY = ev.clientY;
  });

  function endDrag(ev) {
    if (!dragging) return;
    dragging = null;
    canvas.classList.remove('grabbing');
    if (ev && ev.pointerId !== undefined && canvas.hasPointerCapture(ev.pointerId)) {
      canvas.releasePointerCapture(ev.pointerId);
    }
  }
  canvas.addEventListener('pointerup', endDrag);
  canvas.addEventListener('pointercancel', endDrag);

  canvas.addEventListener('wheel', function (ev) {
    ev.preventDefault();
    var p = localPoint(ev);
    var view = r.paneAt(p.x, p.y);
    var z = view.zoom * (ev.deltaY > 0 ? 1.1 : 1 / 1.1);
    // The ceiling is the renderer's, not a constant here: how far a pane can
    // open depends on which projection is drawing it.
    view.zoom = Math.max(0.12, Math.min(r.maxZoom(), z));
  }, { passive: false });

  // Controls.
  // Projection. Set before the layout below, because a fresh pane's width
  // depends on which projection it will be drawn with.
  var proj = document.getElementById('dewarp-projection');
  if (proj) {
    proj.addEventListener('change', function () {
      r.setProjection(parseInt(proj.value, 10) || 0);
    });
    r.setProjection(parseInt(proj.value, 10) || 0);
  }

  var mount = document.getElementById('dewarp-mount');
  if (mount) {
    mount.addEventListener('change', function () {
      r.setMount(mount.value);
    });
    r.setMount(mount.value);
  }

  // Calibration. A lens is not always dead center and not always exactly
  // 180 degrees; a few pixels out shows as a smear at the rim, so these are
  // adjustable rather than assumed.
  function bind(id, apply) {
    var el = document.getElementById(id);
    if (!el) return;
    var out = document.getElementById(id + '-out');
    function sync() {
      var v = parseFloat(el.value);
      apply(v);
      if (out) out.textContent = el.value;
    }
    el.addEventListener('input', sync);
    sync();
  }
  bind('cal-cx', function (v) { r.center[0] = v; });
  bind('cal-cy', function (v) { r.center[1] = v; });
  bind('cal-radius', function (v) { r.radius = v; });
  bind('cal-fov', function (v) { r.halfFov = v * Math.PI / 360; });

  var layout = document.getElementById('dewarp-layout');
  if (layout) {
    layout.addEventListener('change', function () {
      r.setLayout(parseInt(layout.value, 10) || 1);
    });
    r.setLayout(parseInt(layout.value, 10) || 1);
  }

  var reset = document.getElementById('dewarp-reset');
  if (reset) {
    reset.addEventListener('click', function () {
      r.setLayout(r.views.length);
    });
  }

  start();
})();
