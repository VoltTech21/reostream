// Dashboard behavior. Split out of dashboard.html so the template is
// markup and this is behavior; nothing here is new logic, it is the
// script that already lived at the bottom of that page plus the keyframe
// swap.
//
// A camera's tile starts nothing on its own: with eight cameras, creating
// a player per tile at load meant eight simultaneous MPEG-TS decodes the
// instant the page opened. Starting one tile stops whatever tile was
// running, so at most one stream is ever decoded in this tab.

(function () {
  // The prefix the page is served under: "" at the root, or a proxy's or
  // Home Assistant ingress's path. layout.html puts it in a meta tag.
  var pageBase = (document.querySelector('meta[name="reostream-base"]') || {}).content || '';
  // The one live player, when someone has pressed a tile. Null the rest of
  // the time: the rotation below owns the decoder otherwise.
  var live = null;
  // Cameras to walk, in page order.
  var tiles = [];
  var turn = 0;
  var rotating = false;

  // How long to give a camera to produce a frame before moving on, and how
  // long to wait between cameras. A camera that is down must not hold the
  // rotation up.
  var FRAME_WAIT_MS = 6000;
  var BETWEEN_MS = 250;

  function teardown(p) {
    if (!p) return;
    // Pausing is not enough: mpegts.js keeps pulling over XHR while
    // paused, which leaves the daemon streaming to a tab nobody watches.
    try { p.pause(); } catch (e) {}
    try { p.unload(); } catch (e) {}
    try { p.detachMediaElement(); } catch (e) {}
    try { p.destroy(); } catch (e) {}
  }

  // Gives the tile the camera's own shape.
  //
  // Every tile used to be 16:9 with the picture letterboxed inside it, so a
  // camera that is not 16:9 drew at a fraction of the space it was given: a
  // 4096x1152 panoramic filled the width and half the height, with empty
  // hatching above and below, and a 2560x2560 fisheye filled the height and
  // drew a square in the middle of a wide box. Nothing was cropped, but
  // nothing was full size either, and "half the tile is hatching" reads as
  // a broken picture rather than a deliberate one.
  //
  // The shape is taken from the frame rather than from config because only
  // the frame knows: a fisheye changes its own frame size when its Image
  // Layout changes (2560x2560 in one mode, 3200x3200 in another), and no
  // stored value tracks that.
  //
  // Fleet is the exception: there every tile is 16:9 so the cards line up
  // in rows, whatever each camera's shape. A frame near 16:9 fills its
  // tile, trimming at most fleetMaxCrop off the edges; one further off (a
  // panoramic, a fisheye) is drawn whole on black instead, since filling
  // would cut half of it away. Nothing is ever stretched.
  var fleetMaxCrop = 0.15;
  function shapeTile(t, w, h) {
    if (!w || !h || !t.container) return;
    var ratio = w / h;
    if (t.container.closest('.fleet')) {
      var off = ratio / (16 / 9);
      var crop = off > 1 ? 1 - 1 / off : 1 - off;
      t.container.classList.toggle('fit-cover', crop <= fleetMaxCrop);
      t.container.classList.toggle('letterbox', crop > fleetMaxCrop);
      return;
    }
    // Guard rails, not correctness: a camera far outside this range would
    // make one tile tall enough to push the rest of the fleet off screen,
    // or thin enough to be unreadable. Both clamp to a letterbox, which is
    // the old behavior and is the right fallback.
    if (ratio < 0.5) ratio = 0.5;
    if (ratio > 4) ratio = 4;
    t.container.style.aspectRatio = ratio.toFixed(4);
  }

  // A tile whose camera never produced a still -- pressed for live before
  // the rotation reached it -- still gets its shape, from the live video.
  function shapeFromVideo(t) {
    if (!t.video) return;
    t.video.addEventListener('loadedmetadata', function () {
      shapeTile(t, t.video.videoWidth, t.video.videoHeight);
    });
  }

  // Draws whatever the video is showing into the tile's still image.
  function grab(t, video) {
    try {
      if (!video.videoWidth || !video.videoHeight) return false;
      var c = document.createElement('canvas');
      c.width = video.videoWidth;
      c.height = video.videoHeight;
      c.getContext('2d').drawImage(video, 0, 0);
      t.still.src = c.toDataURL('image/jpeg', 0.7);
      t.still.hidden = false;
      shapeTile(t, video.videoWidth, video.videoHeight);
      return true;
    } catch (e) {
      return false;
    }
  }

  // One camera's turn: connect, take a frame, disconnect.
  // The still being taken right now, if any.
  var snapping = null;

  function snap(t, done) {
    if (!window.mpegts || !mpegts.isSupported()) return done();
    var video = t.video;
    // A battery camera with no picture yet has nothing to take a still
    // from, and its live stream would wake it.
    if (!t.src) return done();
    var player = mpegts.createPlayer({type: 'mpegts', isLive: true, url: t.src});
    var finished = false;
    snapping = {finish: finish};

    function finish() {
      if (finished) return;
      finished = true;
      snapping = null;
      clearTimeout(timer);
      video.removeEventListener('loadeddata', onData);
      teardown(player);
      video.hidden = true;
      done();
    }

    function onData() {
      grab(t, video);
      finish();
    }

    var timer = setTimeout(finish, FRAME_WAIT_MS);
    try {
      video.muted = true;
      player.attachMediaElement(video);
      player.load();
      var pr = video.play();
      if (pr && pr.catch) pr.catch(function () {});
      video.addEventListener('loadeddata', onData);
    } catch (e) {
      finish();
    }
  }

  function step() {
    if (!rotating) return;
    if (live || !tiles.length) {
      // Someone is watching something; leave the decoder alone.
      setTimeout(step, 1000);
      return;
    }
    var t = tiles[turn % tiles.length];
    turn++;
    snap(t, function () { setTimeout(step, BETWEEN_MS); });
  }

  function startLive(t) {
    if (!window.mpegts || !mpegts.isSupported()) return;
    // A still being taken uses the same video element; finish it first, or
    // its teardown hides the live picture a moment after it starts.
    if (snapping) snapping.finish();
    stopLive();
    var url = t.live || t.extern || t.src;
    var player = mpegts.createPlayer({type: 'mpegts', isLive: true, url: url});
    player.attachMediaElement(t.video);
    player.load();
    // A battery camera's session ends when reostream puts it back to
    // sleep; show its last frame rather than a frozen player.
    if (t.live) {
      player.on(mpegts.Events.LOADING_COMPLETE, function () { if (live && live.t === t) stopLive(); });
      player.on(mpegts.Events.ERROR, function () { if (live && live.t === t) stopLive(); });
    }
    // A paused live stream keeps buffering, so playing again resumes where
    // it stopped and stays that far behind the camera for good. Jumping to
    // the end of what is buffered puts it back on the live edge. This is
    // on every play, not just the first, because pausing is exactly when
    // it drifts.
    t.video.addEventListener('play', function () {
      try {
        var b = t.video.buffered;
        if (b && b.length) {
          var edge = b.end(b.length - 1);
          if (edge - t.video.currentTime > 1) t.video.currentTime = edge - 0.1;
        }
      } catch (e) {}
    });
    reostreamAudio.play(t.video);
    t.video.hidden = false;
    t.video.setAttribute('controls', '');
    t.still.hidden = true;
    if (t.btn) t.btn.hidden = true;
    // "press for live" is an instruction. While live video is playing it
    // is an instruction to do what has already been done.
    if (t.caption) t.caption.hidden = true;
    live = {player: player, t: t};
  }

  function stopLive() {
    if (!live) return;
    var t = live.t;
    grab(t, t.video);
    teardown(live.player);
    t.video.hidden = true;
    t.video.removeAttribute('controls');
    if (t.btn) t.btn.hidden = false;
    if (t.caption) t.caption.hidden = false;
    live = null;
  }

  document.querySelectorAll('.cam-video[data-src]').forEach(function (container) {
    var t = {
      container: container,
      src: container.dataset.src,
      // A battery camera's still and its live stream are different URLs:
      // the still never wakes it, the live stream does.
      live: container.dataset.live || '',
      extern: container.dataset.extern || '',
      video: container.querySelector('video'),
      still: container.querySelector('img'),
      btn: container.querySelector('.play-btn'),
      caption: container.querySelector('.caption')
    };
    if (!t.video || !t.still) return;
    shapeFromVideo(t);
    tiles.push(t);
    if (t.btn) {
      t.btn.addEventListener('click', function () {
        if (live && live.t === t) {
          stopLive();
        } else {
          startLive(t);
        }
      });
    }
  });

  if (tiles.length) {
    rotating = true;
    step();
  }

  // Nothing decodes while the tab is in the background.
  document.addEventListener('visibilitychange', function () {
    if (document.hidden) {
      stopLive();
      rotating = false;
    } else if (!rotating && tiles.length) {
      rotating = true;
      step();
    }
  });

  // Copying a URL. navigator.clipboard is absent on a page served over
  // plain HTTP to anything but localhost, which is how this page is
  // normally reached, so the fallback is a selection the operator can copy
  // with the keyboard rather than a button that silently does nothing.
  function said(btn, word, back) {
    btn.textContent = word;
    setTimeout(function () { btn.textContent = back; }, 1200);
  }

  function selectURL(btn) {
    var code = btn.parentNode.querySelector('code');
    if (!code) return;
    var range = document.createRange();
    range.selectNodeContents(code);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }

  function copy(text, ok, fail) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(ok, fail);
      return;
    }
    fail();
  }

  document.querySelectorAll('.copy-btn').forEach(function (btn) {
    btn.addEventListener('click', function () {
      copy(btn.dataset.copy, function () { said(btn, 'copied', 'copy'); }, function () { selectURL(btn); });
    });
  });

  // The overlay switches, filled in after the page is on screen. One fetch
  // per camera, each independent: a camera that does not answer leaves its
  // own switches unavailable, with the reason on hover, and costs the rest
  // of the page nothing.
  document.querySelectorAll('.camcard[data-camera]').forEach(loadOSD);

  function loadOSD(card) {
    var camera = card.dataset.camera;
    var forms = card.querySelectorAll('.osd-toggle');
    fetch(pageBase + '/cameras/' + encodeURIComponent(camera) + '/osd', {headers: {'Accept': 'application/json'}})
      .then(function (r) {
        // A non-2xx here is not a camera that failed -- osd.go answers
        // those 200 with a reason -- it is this browser's session having
        // expired, which the auth wrapper answers with a redirect.
        if (!r.ok) throw new Error('not signed in any more (' + r.status + ')');
        return r.json();
      })
      .then(function (flags) {
        forms.forEach(function (form) {
          var field = (flags.fields || {})[form.dataset.xpath];
          if (flags.error) return markUnavailable(form, flags.error);
          if (!field) return markUnavailable(form, 'this camera does not report it');
          if (field.unavailable) return markUnavailable(form, field.unavailable);
          enableToggle(form, flags.block, field.on);
        });
      })
      .catch(function (err) {
        forms.forEach(function (form) { markUnavailable(form, String(err.message || err)); });
      });
  }

  function markUnavailable(form, reason) {
    var label = form.querySelector('label');
    label.classList.add('unavailable');
    label.title = reason;
  }

  function enableToggle(form, block, on) {
    var box = form.querySelector('input[type=checkbox]');
    var label = form.querySelector('label');
    form.querySelector('input[name=block]').value = block;
    box.checked = on;
    box.disabled = false;
    label.title = block;
    box.addEventListener('change', function () {
      // "1" and "0" are what the camera's own document uses for these
      // fields. The write is a normal form post to the settings handler,
      // which reads the document fresh and changes only this field.
      form.querySelector('input[name=value]').value = box.checked ? '1' : '0';
      form.submit();
    });
  }
})();
