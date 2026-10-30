// Hold-to-talk on the camera page: the microphone goes to the camera's
// speaker over a WebSocket as 16 bit PCM, for as long as the button is held.
(function () {
  // The prefix the page is served under: "" at the root, or a proxy's or
  // Home Assistant ingress's path. layout.html puts it in a meta tag.
  var pageBase = (document.querySelector('meta[name="reostream-base"]') || {}).content || '';
  var btn = document.getElementById('talk-button');
  if (!btn) return;
  var status = document.getElementById('talk-status');
  var idle = 'Hold the button and speak.';
  function say(t) { status.textContent = t; }

  if (!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia)) {
    btn.disabled = true;
    say('This browser only allows the microphone on https or localhost. ' +
        'Open the page through an SSH tunnel (http://localhost:8562), or in Chrome add this ' +
        'address under chrome://flags/#unsafely-treat-insecure-origin-as-secure.');
    return;
  }

  var ws = null, ctx = null, src = null, proc = null, stream = null, held = false;

  function stop() {
    held = false;
    btn.classList.remove('talking');
    if (proc) { proc.disconnect(); proc = null; }
    if (src) { src.disconnect(); src = null; }
    if (ctx) { ctx.close(); ctx = null; }
    if (stream) { stream.getTracks().forEach(function (t) { t.stop(); }); stream = null; }
    if (ws) { ws.close(); ws = null; }
  }

  async function start(e) {
    e.preventDefault();
    if (held) return;
    held = true;
    btn.classList.add('talking');
    say('connecting...');
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true } });
    } catch (err) {
      stop();
      say('microphone refused: ' + err.message);
      return;
    }
    if (!held) { stop(); say(idle); return; }

    ctx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 16000 });
    var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(proto + '//' + location.host + pageBase + '/cameras/' +
      encodeURIComponent(btn.dataset.camera) + '/talk?rate=' + Math.round(ctx.sampleRate));
    ws.binaryType = 'arraybuffer';
    ws.onmessage = function (m) { if (typeof m.data === 'string') say(m.data); };
    ws.onclose = function () { if (held) say('the camera connection closed'); };

    src = ctx.createMediaStreamSource(stream);
    proc = ctx.createScriptProcessor(1024, 1, 1);
    proc.onaudioprocess = function (ev) {
      if (!ws || ws.readyState !== 1) return;
      var f = ev.inputBuffer.getChannelData(0);
      var out = new Int16Array(f.length);
      for (var i = 0; i < f.length; i++) {
        var v = Math.max(-1, Math.min(1, f[i]));
        out[i] = v < 0 ? v * 0x8000 : v * 0x7fff;
      }
      ws.send(out.buffer);
    };
    src.connect(proc);
    proc.connect(ctx.destination);
  }

  function release() { if (held) { stop(); say(idle); } }
  btn.addEventListener('pointerdown', start);
  btn.addEventListener('pointerup', release);
  btn.addEventListener('pointerleave', release);
  btn.addEventListener('pointercancel', release);
  btn.addEventListener('contextmenu', function (e) { e.preventDefault(); });
})();
