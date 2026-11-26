// align.js: the full screen seam aligner.
//
// Aligning a seam is a hand-eye task, not a form-filling one. The number
// boxes on the camera page can set the values but cannot tell you whether
// they are right, because judging a seam means looking at the seam, large,
// while it moves. So this page gives the picture the whole window, puts the
// three controls over it, and writes on release rather than on every step of
// a drag -- each write is a round trip to the camera.

(function () {
  'use strict';

  var root = document.getElementById('align');
  if (!root) return;

  var video = document.getElementById('align-video');
  var canvas = document.getElementById('align-canvas');
  var note = document.getElementById('align-note');
  var url = root.getAttribute('data-url');
  var postTo = root.getAttribute('data-post');

  var ctx = canvas.getContext('2d');

  // The seam on a two-lens camera runs down the middle of the frame. It is
  // drawn rather than left to be found by eye: the whole adjustment is
  // judging what happens AT the seam, and a line through it is the
  // difference between comparing two halves and hunting for the join.
  var showGuide = true;
  // Zoom crops to a band around the seam. At 4096 wide on a laptop screen
  // the seam is a couple of hundred pixels of a four thousand pixel frame,
  // which is far too small to judge.
  var zoom = 1;

  // How far the crop has been dragged from center, in SOURCE pixels, and
  // the scale the last frame was drawn at so a drag in canvas pixels can be
  // converted back.
  var panX = 0, panY = 0, lastScale = 1;

  var player = null;
  function start() {
    if (player || !window.mpegts || !mpegts.isSupported()) {
      if (!window.mpegts || !mpegts.isSupported()) {
        note.textContent = 'This browser cannot play MPEG-TS, so there is no picture to align against.';
      }
      return;
    }
    player = mpegts.createPlayer({ type: 'mpegts', isLive: true, url: url }, {enableStashBuffer: false}); // live, as reostreamLive in snapshot.js, which this page does not load
    player.attachMediaElement(video);
    player.load();
    var p = video.play();
    if (p && p.catch) p.catch(function () { /* autoplay refusal is not fatal */ });
  }

  function stop() {
    if (!player) return;
    try { player.destroy(); } catch (e) { /* already gone */ }
    player = null;
  }

  // Which stream is actually on screen, said out loud.
  //
  // It matters here more than anywhere else on this page: judging a seam is
  // judging fine detail, and the best stream is often not available. This
  // camera's main stream is HEVC, which no browser will decode, so what
  // arrives is the balanced stream at 2560x720 rather than the 4096x1152
  // being recorded. Someone deciding whether a seam is right should know
  // they are looking at 62% of the width it is stored at.
  var said = false;
  function sayStream() {
    if (said || !video.videoWidth) return;
    said = true;
    var name = (url.split('/').pop() || '').replace('.ts', '');
    note.textContent = 'Judging ' + name + ' at ' + video.videoWidth + '×' +
      video.videoHeight + '. Drag a control and let go — it writes on release, ' +
      'because each change is a round trip to the camera.';
  }

  var running = true;
  function frame() {
    if (!running) return;
    draw();
    sayStream();
    requestAnimationFrame(frame);
  }

  function draw() {
    var vw = video.videoWidth, vh = video.videoHeight;
    if (!vw || !vh) return;

    var cw = canvas.clientWidth | 0, ch = canvas.clientHeight | 0;
    if (!cw || !ch) return;
    var dpr = Math.min(window.devicePixelRatio || 1, 2);
    if (canvas.width !== (cw * dpr | 0) || canvas.height !== (ch * dpr | 0)) {
      canvas.width = cw * dpr | 0;
      canvas.height = ch * dpr | 0;
    }
    var W = canvas.width, H = canvas.height;

    // Source rectangle: the whole frame at zoom 1, a band centered on the
    // seam as it goes up.
    var sw = vw / zoom, sh = vh / zoom;
    // Centered, then moved by however far the picture has been dragged, and
    // clamped so the crop can never leave the frame. Without the pan a zoom
    // could only ever show the middle: the seam runs the full height of the
    // picture, and the interesting part of it is rarely dead center.
    var maxX = vw - sw, maxY = vh - sh;
    var sx = Math.max(0, Math.min(maxX, (vw - sw) / 2 + panX));
    var sy = Math.max(0, Math.min(maxY, (vh - sh) / 2 + panY));
    // Re-seat the stored pan on the clamped result, so dragging past an
    // edge does not build up slack that has to be dragged back out.
    panX = sx - (vw - sw) / 2;
    panY = sy - (vh - sh) / 2;
    // Fit that band into the canvas without distorting it. A stretched
    // picture would make a straight seam look bent, which is the one thing
    // this page must never do. The scale is kept for the pan maths, so one
    // screen pixel of drag moves one screen pixel of picture at any zoom.
    var scale = Math.min(W / sw, H / sh) || 1;
    lastScale = scale;
    var dw = sw * scale, dh = sh * scale;
    var dx = (W - dw) / 2, dy = (H - dh) / 2;

    ctx.fillStyle = '#000';
    ctx.fillRect(0, 0, W, H);
    ctx.drawImage(video, sx, sy, sw, sh, dx, dy, dw, dh);

    if (showGuide) {
      // The seam is at the middle of the FRAME, not the middle of the
      // canvas. Those are the same place only while the picture is
      // centered, so the line is mapped through the crop: without this it
      // stays put on screen while the picture slides out from under it,
      // and panning to look at one end of the seam leaves the guide
      // marking nothing.
      var gx = dx + (vw / 2 - sx) * scale;
      if (gx >= dx && gx <= dx + dw) {
        ctx.save();
        ctx.strokeStyle = 'rgba(80, 255, 180, 0.65)';
        ctx.lineWidth = Math.max(1, dpr);
        ctx.setLineDash([10 * dpr, 8 * dpr]);
        ctx.beginPath();
        ctx.moveTo(gx, dy);
        ctx.lineTo(gx, dy + dh);
        ctx.stroke();
        ctx.restore();
      }
    }
  }

  requestAnimationFrame(frame);
  window.addEventListener('pagehide', function () { running = false; stop(); });

  // A hidden tab stops streaming, and starts again when it comes back. See
  // the same guard in fisheyeview.js: pagehide fires on navigation and
  // close but not on a backgrounded tab, and a live MediaSource buffer left
  // filling for hours is measured in gigabytes.
  document.addEventListener('visibilitychange', function () {
    if (document.hidden) {
      stop();
      said = false; // so the stream readout is rebuilt on the way back
    } else if (running) {
      start();
    }
  });

  // Controls. Each field has a slider and a number, kept in step, and both
  // write only when the interaction ends.
  var fields = ['distance', 'x', 'y'];
  var pending = false;

  function valuesNow() {
    var v = {};
    fields.forEach(function (f) {
      var el = document.getElementById('al-' + f);
      if (el) v[f] = el.value;
    });
    return v;
  }

  function send() {
    if (pending) return;
    pending = true;
    note.textContent = 'sending…';
    var body = new URLSearchParams(valuesNow());
    fetch(postTo, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: body.toString(),
      // The handler answers with a redirect to the camera page; this page
      // wants the outcome, not the navigation.
      redirect: 'manual'
    }).then(function () {
      note.textContent = 'sent. The seam moves on the live picture within a second or two.';
    }).catch(function (e) {
      note.textContent = 'the camera did not take it: ' + e.message;
    }).then(function () {
      pending = false;
    });
  }

  // Put one field's value into all three of the things that show it: the
  // number box, the slider and the readout. Anything that changes a value
  // from outside the controls themselves -- the undo buttons below -- goes
  // through here, so the slider never ends up pointing somewhere the number
  // does not say.
  function setField(f, v) {
    var num = document.getElementById('al-' + f);
    var rng = document.getElementById('al-' + f + '-range');
    var out = document.getElementById('al-' + f + '-out');
    if (num) num.value = v;
    if (rng) rng.value = v;
    if (out) out.textContent = v;
  }

  fields.forEach(function (f) {
    var num = document.getElementById('al-' + f);
    var rng = document.getElementById('al-' + f + '-range');
    if (!num) return;
    function sync(from, to) {
      if (to && to.value !== from.value) to.value = from.value;
      var out = document.getElementById('al-' + f + '-out');
      if (out) out.textContent = from.value;
    }
    if (rng) {
      // input keeps the readout live under the thumb; change fires when the
      // drag ends, which is when it is worth a round trip to the camera.
      rng.addEventListener('input', function () { sync(rng, num); });
      rng.addEventListener('change', function () { sync(rng, num); send(); });
    }
    num.addEventListener('input', function () { sync(num, rng); });
    num.addEventListener('change', function () { sync(num, rng); send(); });
    sync(num, rng);
  });

  // The two undo buttons. They set all three fields and send, because on
  // this page the picture is the feedback: a reset that only filled the
  // boxes in would leave the seam where it was and the operator guessing
  // whether anything had happened.
  function undoTo(b) {
    fields.forEach(function (f) {
      if (b.dataset[f] !== undefined) setField(f, b.dataset[f]);
    });
    send();
  }

  ['revert', 'factory'].forEach(function (which) {
    var b = document.getElementById('al-' + which);
    if (b) b.addEventListener('click', function () { undoTo(b); });
  });

  var guide = document.getElementById('al-guide');
  if (guide) {
    guide.addEventListener('change', function () { showGuide = guide.checked; });
    showGuide = guide.checked;
  }

  // Drag the picture to pan while zoomed in. Pointer events rather than
  // mouse events so a touchscreen works; the canvas already sets
  // touch-action so the page does not scroll underneath the drag.
  var dragging = false, lastX = 0, lastY = 0;

  canvas.addEventListener('pointerdown', function (ev) {
    if (zoom <= 1) return;           // nothing to pan at full frame
    dragging = true;
    lastX = ev.clientX;
    lastY = ev.clientY;
    canvas.setPointerCapture(ev.pointerId);
    canvas.classList.add('grabbing');
  });

  canvas.addEventListener('pointermove', function (ev) {
    if (!dragging) return;
    // Dragging right should move the picture right, which means moving the
    // crop LEFT, hence the subtraction. Divided by the draw scale so one
    // screen pixel of drag moves one screen pixel of picture at any zoom.
    panX -= (ev.clientX - lastX) / lastScale;
    panY -= (ev.clientY - lastY) / lastScale;
    lastX = ev.clientX;
    lastY = ev.clientY;
  });

  function endPan(ev) {
    if (!dragging) return;
    dragging = false;
    canvas.classList.remove('grabbing');
    if (ev && ev.pointerId !== undefined && canvas.hasPointerCapture(ev.pointerId)) {
      canvas.releasePointerCapture(ev.pointerId);
    }
  }
  canvas.addEventListener('pointerup', endPan);
  canvas.addEventListener('pointercancel', endPan);

  var zoomEl = document.getElementById('al-zoom');
  if (zoomEl) {
    function applyZoom() {
      zoom = parseFloat(zoomEl.value) || 1;
      // Back at full frame there is nowhere to pan, so forget the offset
      // rather than leaving the next zoom-in starting somewhere unexpected.
      if (zoom <= 1) { panX = 0; panY = 0; }
      canvas.style.cursor = zoom > 1 ? 'grab' : 'default';
      var out = document.getElementById('al-zoom-out');
      if (out) out.textContent = zoom.toFixed(1) + '×';
    }
    zoomEl.addEventListener('input', applyZoom);
    applyZoom();
  }

  start();
})();
