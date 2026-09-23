// Dashboard behaviour. Split out of dashboard.html so the template is
// markup and this is behaviour; nothing here is new logic, it is the
// script that already lived at the bottom of that page plus the keyframe
// swap.
//
// A camera's tile starts nothing on its own: with eight cameras, creating
// a player per tile at load meant eight simultaneous MPEG-TS decodes the
// instant the page opened. Starting one tile stops whatever tile was
// running, so at most one stream is ever decoded in this tab.

(function () {
  var active = null;

  function stopActive() {
    if (!active) return;
    // Pausing the <video> is not enough: mpegts.js keeps pulling over XHR
    // while paused, which leaves the daemon streaming to a tab nobody is
    // watching. destroy() closes the fetch.
    try { active.player.pause(); } catch (e) {}
    try { active.player.unload(); } catch (e) {}
    try { active.player.detachMediaElement(); } catch (e) {}
    active.player.destroy();
    active.video.hidden = true;
    active.video.removeAttribute('controls');
    if (active.still) active.still.hidden = false;
    active.btn.hidden = false;
    active = null;
  }

  function startPlayer(container) {
    if (!window.mpegts || !mpegts.isSupported()) return;
    stopActive();
    var video = container.querySelector('video');
    var btn = container.querySelector('.play-btn');
    var still = container.querySelector('img');
    var player = mpegts.createPlayer({type: 'mpegts', isLive: true, url: container.dataset.src});
    player.attachMediaElement(video);
    player.load();
    player.play();
    video.hidden = false;
    video.setAttribute('controls', '');
    if (still) still.hidden = true;
    btn.hidden = true;
    active = {player: player, video: video, btn: btn, still: still};
  }

  document.querySelectorAll('.cam-video[data-src]').forEach(function (container) {
    var btn = container.querySelector('.play-btn');
    if (btn) btn.addEventListener('click', function () { startPlayer(container); });
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

  var all = document.getElementById('copy-all');
  if (all) {
    all.addEventListener('click', function () {
      var urls = [];
      document.querySelectorAll('.copy-btn').forEach(function (b) { urls.push(b.dataset.copy); });
      copy(urls.join('\n'), function () { said(all, 'copied', 'Copy all URLs'); }, function () {});
    });
  }

  // The overlay switches, filled in after the page is on screen. One fetch
  // per camera, each independent: a camera that does not answer leaves its
  // own switches unavailable, with the reason on hover, and costs the rest
  // of the page nothing.
  document.querySelectorAll('.camcard[data-camera]').forEach(loadOSD);

  function loadOSD(card) {
    var camera = card.dataset.camera;
    var forms = card.querySelectorAll('.osd-toggle');
    fetch('/cameras/' + encodeURIComponent(camera) + '/osd', {headers: {'Accept': 'application/json'}})
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
