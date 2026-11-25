// Camera page behavior.
//
// Two things here. A two-state setting is a switch, not a dropdown, and a
// switch posts the same form the dropdown did. And a position select whose
// switch is off is dimmed and disabled, because choosing a corner for an
// overlay that is not drawn is a write with no effect.

(function () {
  // Saving. Every form on a settings page posts in the background and the
  // result shows as a short note; nothing reloads and no control is
  // redrawn, so the live preview keeps playing and the row stays put.
  //
  // send() goes through requestSubmit, so a form's own confirm still comes
  // first; a canceled confirm puts the control back.
  function send(form, undo) {
    form._undo = undo;
    form.requestSubmit();
  }

  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (!form.closest('.cam-pane') || form.hasAttribute('data-nav')) return;
    var undo = form._undo;
    form._undo = null;
    if (e.defaultPrevented) {
      if (undo) undo();
      return;
    }
    e.preventDefault();
    post(form, undo);
  });

  var note = null, noteTimer = null;
  function say(text, bad) {
    if (!note) {
      note = document.createElement('div');
      note.className = 'save-note';
      note.setAttribute('role', 'status');
      document.body.appendChild(note);
    }
    note.textContent = text;
    note.classList.toggle('bad', !!bad);
    note.classList.add('show');
    clearTimeout(noteTimer);
    noteTimer = setTimeout(function () { note.classList.remove('show'); }, bad ? 6000 : 2200);
  }

  function post(form, undo) {
    say('Saving\u2026');
    fetch(form.action || location.pathname, {
      method: 'POST',
      body: new URLSearchParams(new FormData(form)),
      headers: {'Accept': 'text/html'},
      credentials: 'same-origin'
    }).then(function (resp) {
      if (!resp.ok) return resp.text().then(function (t) { throw new Error(t.trim() || ('status ' + resp.status)); });
      return resp.text();
    }).then(function (html) {
      var doc = new DOMParser().parseFromString(html, 'text/html');
      var flash = doc.querySelector('.flash');
      var refused = flash && flash.classList.contains('flash-refused');
      // "Accepted" is the camera answering OK and then, read back, still
      // holding the old value: not saved, whatever its answer said. Say
      // so, and let refresh show what the camera really holds.
      var unconfirmed = flash && flash.classList.contains('flash-accepted');
      if (refused && undo) undo();
      var text = flash ? flash.querySelector('.flash-text').textContent.trim() : 'Saved';
      say(refused || unconfirmed || form.hasAttribute('data-report') ? text : 'Saved', refused || unconfirmed);
      // data-keep: the form already shows what was saved and holds a
      // player and a painter this file did not wire, which a swapped-in
      // copy would lose (the zone editor, #47).
      if (!refused && !form.hasAttribute('data-keep')) refresh(form, doc);
      governs();
    }).catch(function (err) {
      if (undo) undo();
      say('Not saved: ' + err.message, true);
      governs();
    });
  }

  // A switch names the setting it governs with data-governs; that row is
  // dimmed while the switch is off.
  function governs() {
    document.querySelectorAll('.switch[data-governs]').forEach(function (sw) {
      var row = document.getElementById(sw.dataset.governs);
      if (!row) return;
      var off = sw.getAttribute('aria-pressed') !== 'true';
      row.classList.toggle('off', off);
      row.querySelectorAll('select, button, input').forEach(function (el) { el.disabled = off; });
    });
  }

  // Every binding below runs again after a save swaps the settings
  // sections in: the elements it bound to were replaced, and a control
  // with no handler looks identical to one that works until it is
  // pressed. The camera tile is bound outside this, because it is not
  // swapped and must keep the player it already has.
  // refresh puts the row the camera now reports in place of the one that
  // was saved, so its changed-from-factory dot and reset button say what
  // is true after the save, not before it. The reply to a save is the
  // settings page read fresh from the camera; the same form is found in it
  // by its action and its position among forms sharing that action. A
  // flash's undo form posts to the same action and comes first in the
  // reply, so it is never counted: counted, it took the saved control's
  // place, and the floodlight's four stops turned into a lone undo (#56).
  function refresh(form, doc) {
    var action = form.getAttribute('action');
    if (!action) return;
    var sel = 'form[action="' + action + '"]:not(.flash-undo)';
    var mine = Array.prototype.indexOf.call(document.querySelectorAll(sel), form);
    var fresh = doc.querySelectorAll(sel)[mine];
    if (mine < 0 || !fresh) return;
    var node = document.importNode(fresh, true);
    form.replaceWith(node);
    wire(node);
    masters();
  }

  function wire(root) {
    root = root || document;
    // A switch writes 1 or 0 into its field (value, unless data-field names
    // another) and saves, unless data-hold says the form's own set button
    // does that.
    root.querySelectorAll('.switch').forEach(function (sw) {
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
    // A data-tool group only picks how the page behaves (the zone editor's
    // brush) and saves nothing: wired like a setting, picking Detect to
    // clear an ignored area saved the zone on the spot and swapped the
    // editor out from under the pointer, playing nothing (#47).
    root.querySelectorAll('.segmented:not([data-tool])').forEach(function (group) {
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
    root.querySelectorAll('.reset[data-default]').forEach(function (btn) {
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

    governs();

    // A typed value outside the field's range is never sent. It used to be
    // blocked silently and left in the box, so the box showed a value the
    // camera never got. Now the browser says why, and the box goes back to
    // the value the camera holds.
    function outOfRange(box, saved) {
      if (box.checkValidity()) return false;
      var min = box.getAttribute('min'), max = box.getAttribute('max');
      box.setCustomValidity(min !== null && max !== null ? 'Use a value from ' + min + ' to ' + max + '.' : '');
      box.reportValidity();
      box.setCustomValidity('');
      box.value = saved;
      return true;
    }

    // Keeps each slider and its number box showing the same value. The box
    // is authoritative -- it is the input that posts -- so the slider
    // follows it only when the typed value is one the track can represent;
    // a value outside the track is left exactly as typed.
    root.querySelectorAll('.slider').forEach(function (wrap) {
      var range = wrap.querySelector('input[type=range]');
      var box = wrap.querySelector('input[type=number]');
      if (!range || !box) return;
      var saved = box.value;
      var save = function () {
        if (box.value === saved || !box.form) return;
        if (outOfRange(box, saved)) { range.value = saved; return; }
        var before = saved;
        saved = box.value;
        send(box.form, function () { box.value = before; range.value = before; saved = before; });
      };
      range.addEventListener('input', function () { box.value = range.value; });
      range.addEventListener('change', save);
      box.addEventListener('input', function () {
        var v = Number(box.value);
        if (box.value !== '' && !isNaN(v) && v >= Number(range.min) && v <= Number(range.max)) range.value = box.value;
      });
      box.addEventListener('change', save);
    });

    // A dropdown saves when a new value is picked.
    root.querySelectorAll('.autosave select').forEach(function (sel) {
      var saved = sel.value;
      sel.addEventListener('change', function () {
        var before = saved;
        saved = sel.value;
        send(sel.form, function () { sel.value = before; saved = before; });
      });
    });

    // Text and number fields save when they lose focus or on Enter.
    root.querySelectorAll('.autosave input[type=text], .autosave input[type=number]:not(.slider-box)').forEach(function (input) {
      var saved = input.value;
      input.addEventListener('change', function () {
        if (input.value === saved) return;
        if (outOfRange(input, saved)) return;
        var before = saved;
        saved = input.value;
        send(input.form, function () { input.value = before; saved = before; });
      });
      input.addEventListener('keydown', function (e) {
        if (e.key === 'Enter') { e.preventDefault(); input.blur(); }
      });
    });
  }

  // The per-section reset. It does not know how to write anything: it
  // clicks the rows' own reset buttons, so a reset is one code path
  // whether it came from a row or from the heading. It appears only in
  // sections that have something to put back, and the rows are looked up
  // on each click, since a save can change which rows have one.
  document.querySelectorAll('section').forEach(function (section) {
    var master = section.querySelector('.section-reset');
    if (!master) return;
    master.textContent = '↺ Reset section';
    master.addEventListener('click', function () {
      var heading = section.querySelector('h2');
      var name = heading ? heading.textContent.trim() : 'this section';
      if (!confirm('Reset ' + name + ' to factory?')) {
        return;
      }
      section.querySelectorAll('.reset[data-default]').forEach(function (btn) { btn.click(); });
    });
  });

  function masters() {
    document.querySelectorAll('section').forEach(function (section) {
      var master = section.querySelector('.section-reset');
      if (master) master.hidden = !section.querySelector('.reset[data-default]');
    });
  }

  wire();
  masters();

  // Copy to other cameras: the link opens the panel, "All" ticks every box.
  document.querySelectorAll('.copy-open').forEach(function (b) {
    var panel = b.closest('section').querySelector('.copy-panel');
    if (!panel) return;
    b.addEventListener('click', function () {
      panel.hidden = !panel.hidden;
      b.setAttribute('aria-expanded', panel.hidden ? 'false' : 'true');
    });
    var all = panel.querySelector('[data-all]');
    all.addEventListener('change', function () {
      panel.querySelectorAll('input[name=to]').forEach(function (c) { c.checked = all.checked; });
    });
  });


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
    // The settings preview takes the picture's own shape, so a panoramic
    // or a fisheye is shown whole instead of cropped to a 16:9 box.
    if (container.closest('.sec-preview')) {
      video.addEventListener('loadedmetadata', function () {
        if (video.videoWidth && video.videoHeight)
          container.style.aspectRatio = (video.videoWidth / video.videoHeight).toFixed(4);
      });
    }
  }

  if (container.dataset.autoplay) {
    video.muted = true;
    // The settings preview is a picture to judge by, not a player.
    start(!container.closest('.sec-preview'));
  } else if (btn) {
    btn.addEventListener('click', function () { start(true); });
  }
})();


// Lens alignment: the reset controls.
//
// Three fields, and two different senses of "undo" that answer different
// questions. The per-field arrow and the Factory button put back the
// camera's own shipped value, which is where to go when nudging has gone
// far enough wrong that "before" is not far enough back. Revert puts back
// what the camera actually held when this page was drawn, which is what
// somebody wants after a few nudges that lost track of the starting point.
//
// Each fills the form in and saves it, the same as any other control.
(function () {
  'use strict';

  var note = document.getElementById('stitch-note');
  function say(msg) {
    if (!note) return;
    note.textContent = msg;
  }

  // One field, from the arrow beside it.
  document.addEventListener('click', function (e) {
    var b = e.target.closest('.stitch-reset');
    if (!b) return;
    var input = document.getElementById(b.dataset.target);
    if (!input) return;
    input.value = b.dataset.value;
    say('');
    var f = input.form; if (f) f.requestSubmit();
  });

  // All three at once, from either button. Both carry their values as data
  // attributes rendered by the template, so this needs no knowledge of what
  // the camera reported.
  function applyAll(b, what) {
    ['distance', 'x', 'y'].forEach(function (f) {
      var input = document.getElementById('st-' + f);
      if (input && b.dataset[f] !== undefined) input.value = b.dataset[f];
    });
    say('');
    var f = b.closest('form'); if (f) f.requestSubmit();
  }

  var revert = document.getElementById('stitch-revert');
  if (revert) {
    revert.addEventListener('click', function () {
      applyAll(revert, 'back to the values this page opened with');
    });
  }

  var factory = document.getElementById('stitch-factory');
  if (factory) {
    factory.addEventListener('click', function () {
      applyAll(factory, 'back to the factory alignment');
    });
  }
})();
