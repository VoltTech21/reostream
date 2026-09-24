// Camera page behaviour.
//
// Two things here. A two-state setting is a switch, not a dropdown, and a
// switch posts the same form the dropdown did. And a position select whose
// switch is off is dimmed and disabled, because choosing a corner for an
// overlay that is not drawn is a write with no effect.

(function () {
  // Submits through the form's own onsubmit, so a confirmation the form
  // asks for still comes first. form.submit() would skip it.
  //
  // requestSubmit runs the inline onsubmit; if that cancelled, the event
  // arrives already defaultPrevented and nothing is sent. Otherwise this
  // cancels the navigation itself and posts the same form by fetch, so the
  // live tile survives a save.
  function send(form, undo) {
    form.addEventListener('submit', function once(e) {
      form.removeEventListener('submit', once);
      if (e.defaultPrevented) {
        if (undo) undo();
        return;
      }
      e.preventDefault();
      post(form, undo);
    });
    form.requestSubmit();
  }

  // Posts the form and swaps in the parts of the reply that can have
  // changed. Anything this cannot do in place falls back to navigating,
  // because a save whose result is never shown is worse than a reload.
  function post(form, undo) {
    var body = new URLSearchParams(new FormData(form));
    fetch(form.action || location.pathname, {
      method: 'POST',
      body: body,
      headers: {'Accept': 'text/html'},
      credentials: 'same-origin'
    }).then(function (resp) {
      if (!resp.ok) throw new Error('status ' + resp.status);
      return resp.text();
    }).then(function (html) {
      var doc = new DOMParser().parseFromString(html, 'text/html');
      if (!swap(doc)) {
        location.reload();
        return;
      }
      wire();
    }).catch(function () {
      // The write may still have happened, so the page has to be told the
      // truth by the server rather than guessed at here.
      if (undo) undo();
      location.reload();
    });
  }

  // Replaces the settings sections and the flash from a freshly rendered
  // copy of this page. The camera card is left alone: the video element is
  // inside it, and replacing it is exactly what this exists to avoid.
  function swap(doc) {
    var to = document.querySelectorAll('.camera-cols section');
    var from = doc.querySelectorAll('.camera-cols section');
    if (!to.length || to.length !== from.length) return false;
    for (var i = 0; i < to.length; i++) {
      to[i].replaceWith(from[i]);
    }
    var oldFlash = document.querySelector('.flash');
    var newFlash = doc.querySelector('.flash');
    if (oldFlash && newFlash) {
      oldFlash.replaceWith(newFlash);
    } else if (newFlash) {
      var cols = document.querySelector('.camera-cols');
      if (cols && cols.parentNode) cols.parentNode.insertBefore(newFlash, cols);
    } else if (oldFlash) {
      oldFlash.remove();
    }
    return true;
  }

  // Every binding below runs again after a save swaps the settings
  // sections in: the elements it bound to were replaced, and a control
  // with no handler looks identical to one that works until it is
  // pressed. The camera tile is bound outside this, because it is not
  // swapped and must keep the player it already has.
  function wire() {
    // A switch writes 1 or 0 into its field (value, unless data-field names
    // another) and saves, unless data-hold says the form's own set button
    // does that.
    document.querySelectorAll('.switch').forEach(function (sw) {
      sw.addEventListener('click', function () {
        var on = sw.getAttribute('aria-pressed') === 'true';
        var form = sw.closest('form');
        if (!form) return;
        var field = form.querySelector('input[name=' + (sw.dataset.field || 'value') + ']');
        var set = function (v) {
          sw.setAttribute('aria-pressed', v ? 'true' : 'false');
          if (field) field.value = v ? '1' : '0';
        };
        set(!on);
        if (sw.hasAttribute('data-hold')) return;
        send(form, function () { set(on); });
      });
    });

    // Stops (an overlay's corner, the floodlight): choosing one saves it.
    document.querySelectorAll('.segmented').forEach(function (group) {
      var was = group.querySelector('input:checked');
      group.querySelectorAll('input[type=radio]').forEach(function (radio) {
        radio.addEventListener('change', function () {
          var form = radio.closest('form');
          if (!form) return;
          send(form, function () {
            if (was) was.checked = true; else radio.checked = false;
          });
        });
      });
    });

    // Reset: put the camera's factory value into whatever control this form
    // posts through, then save it the same way that control would.
    document.querySelectorAll('.reset[data-default]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var form = btn.closest('form');
        if (!form) return;
        var def = btn.dataset.default;
        var radios = form.querySelectorAll('input[type=radio][name=value]');
        var box = form.querySelector('input[name=value]:not([type=radio])');
        var sw = form.querySelector('.switch');
        var range = form.querySelector('input[type=range]');
        var undo;
        if (radios.length) {
          var was = form.querySelector('input[type=radio][name=value]:checked');
          radios.forEach(function (r) { r.checked = r.value === def; });
          undo = function () { radios.forEach(function (r) { r.checked = r === was; }); };
        } else if (box) {
          var before = box.value;
          box.value = def;
          if (range) range.value = def;
          if (sw) sw.setAttribute('aria-pressed', def === '1' ? 'true' : 'false');
          undo = function () {
            box.value = before;
            if (range) range.value = before;
            if (sw) sw.setAttribute('aria-pressed', before === '1' ? 'true' : 'false');
          };
        } else {
          return;
        }
        send(form, undo);
      });
    });

    // A switch names the setting it governs with data-governs; that row is
    // dimmed while the switch is off.
    document.querySelectorAll('.switch[data-governs]').forEach(function (sw) {
      var row = document.getElementById(sw.dataset.governs);
      if (!row) return;
      var off = sw.getAttribute('aria-pressed') !== 'true';
      row.classList.toggle('off', off);
      row.querySelectorAll('select, button, input').forEach(function (el) { el.disabled = off; });
    });

    // Keeps each slider and its number box showing the same value. The box
    // is authoritative -- it is the input that posts -- so the slider
    // follows it only when the typed value is one the track can represent;
    // a value outside the track is left exactly as typed.
    document.querySelectorAll('.slider').forEach(function (wrap) {
      var range = wrap.querySelector('input[type=range]');
      var box = wrap.querySelector('input[type=number]');
      if (!range || !box) return;
      range.addEventListener('input', function () { box.value = range.value; });
      box.addEventListener('input', function () {
        var v = Number(box.value);
        if (box.value !== '' && !isNaN(v) && v >= Number(range.min) && v <= Number(range.max)) range.value = box.value;
      });
    });

    // The per-section reset. It does not know how to write anything: it
    // clicks the rows' own reset buttons, so a reset is one code path
    // whether it came from a row or from the heading. It appears only in
    // sections that have something to put back, and says how many, because
    // "reset section" with no number is a button nobody can predict.
    document.querySelectorAll('section').forEach(function (section) {
      var master = section.querySelector('.section-reset');
      if (!master) return;
      var rows = section.querySelectorAll('.reset[data-default]');
      if (!rows.length) return;
      master.hidden = false;
      master.textContent = '↺ reset ' + rows.length +
        (rows.length === 1 ? ' setting' : ' settings') + ' to factory';
      master.addEventListener('click', function () {
        var heading = section.querySelector('h2');
        var name = heading ? heading.textContent.trim() : 'this section';
        if (!confirm('Put ' + rows.length + ' ' + name.toLowerCase() +
            (rows.length === 1 ? ' setting' : ' settings') +
            ' back to the camera’s factory values?')) {
          return;
        }
        rows.forEach(function (btn) { btn.click(); });
      });
    });
  }

  wire();


  // The single camera tile, same discipline as the dashboard: nothing
  // decodes until the tile is pressed.
  var container = document.querySelector('.cam-video[data-src]');
  if (!container) return;
  var btn = container.querySelector('.play-btn');
  var video = container.querySelector('video');
  var still = container.querySelector('img');
  // One camera, one tile: there is nothing to compete with it, so it
  // plays on its own rather than asking for a press first. Muted, because
  // a browser refuses to autoplay anything else.
  function start(controls) {
    if (!window.mpegts || !mpegts.isSupported()) return;
    var player = reostreamPlayer(container.dataset.src, video);
    player.attachMediaElement(video);
    player.load();
    var p = video.play();
    if (p && p.catch) p.catch(function () {});
    video.hidden = false;
    if (controls) video.setAttribute('controls', '');
    if (still) still.hidden = true;
    if (btn) btn.hidden = true;
  }

  if (container.dataset.autoplay) {
    video.muted = true;
    start(true);
  } else if (btn) {
    btn.addEventListener('click', function () { start(true); });
  }
})();
