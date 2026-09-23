// Camera page behaviour.
//
// Two things here. A two-state setting is a switch, not a dropdown, and a
// switch posts the same form the dropdown did. And a position select whose
// switch is off is dimmed and disabled, because choosing a corner for an
// overlay that is not drawn is a write with no effect.

(function () {
  // Submits through the form's own onsubmit, so a confirmation the form
  // asks for still comes first. form.submit() would skip it.
  function send(form, undo) {
    form.addEventListener('submit', function once(e) {
      form.removeEventListener('submit', once);
      if (e.defaultPrevented) undo();
    });
    form.requestSubmit();
  }

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

  // The single camera tile, same discipline as the dashboard: nothing
  // decodes until the tile is pressed.
  var container = document.querySelector('.cam-video[data-src]');
  if (!container) return;
  var btn = container.querySelector('.play-btn');
  var video = container.querySelector('video');
  var still = container.querySelector('img');
  if (btn) btn.addEventListener('click', function () {
    if (!window.mpegts || !mpegts.isSupported()) return;
    var player = mpegts.createPlayer({type: 'mpegts', isLive: true, url: container.dataset.src});
    player.attachMediaElement(video);
    player.load();
    player.play();
    video.hidden = false;
    video.setAttribute('controls', '');
    if (still) still.hidden = true;
    btn.hidden = true;
  });
})();
