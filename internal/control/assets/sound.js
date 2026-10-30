// Custom siren sound (camera.html "sound"): decode the chosen file in the
// browser, mix to mono, resample to 16 kHz, cut to 5 s, and post the 16-bit
// samples; the server encodes and uploads them.
(function () {
  // The prefix the page is served under: "" at the root, or a proxy's or
  // Home Assistant ingress's path. layout.html puts it in a meta tag.
  var pageBase = (document.querySelector('meta[name="reostream-base"]') || {}).content || '';
  document.querySelectorAll('section.sound').forEach(function (sec) {
    var cam = sec.dataset.camera;
    var file = sec.querySelector('.sound-file');
    var up = sec.querySelector('.sound-upload');
    var test = sec.querySelector('.sound-test');
    var msg = sec.querySelector('.sound-msg');
    function say(t) { msg.textContent = t; }
    file.addEventListener('change', function () { up.disabled = !file.files.length; });
    up.addEventListener('click', function () {
      var f = file.files[0];
      if (!f) return;
      up.disabled = true;
      say('Preparing the sound...');
      f.arrayBuffer().then(function (buf) {
        var AC = window.AudioContext || window.webkitAudioContext;
        return new AC().decodeAudioData(buf);
      }).then(function (audio) {
        var secs = Math.min(audio.duration, 5);
        var off = new OfflineAudioContext(1, Math.ceil(secs * 16000), 16000);
        var src = off.createBufferSource();
        src.buffer = audio;
        src.connect(off.destination);
        src.start(0);
        return off.startRendering();
      }).then(function (rendered) {
        var f32 = rendered.getChannelData(0);
        var pcm = new Int16Array(f32.length);
        for (var i = 0; i < f32.length; i++) {
          var v = Math.max(-1, Math.min(1, f32[i]));
          pcm[i] = v < 0 ? v * 32768 : v * 32767;
        }
        say('Uploading to the camera...');
        return fetch(pageBase + '/cameras/' + encodeURIComponent(cam) + '/sound', {
          method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body: pcm.buffer
        });
      }).then(function (r) { return r.json(); }).then(function (j) {
        say(j.ok ? 'Done: ' + j.detail + '.' : 'Not saved: ' + j.detail);
      }).catch(function (e) {
        say('Could not read that file as audio (' + e + ').');
      }).then(function () { up.disabled = !file.files.length; });
    });
    test.addEventListener('click', function () {
      if (!confirm('Play the siren sound out loud on the camera?')) return;
      fetch(pageBase + '/cameras/' + encodeURIComponent(cam) + '/sound/test', { method: 'POST' })
        .then(function (r) { return r.json(); })
        .then(function (j) { say(j.ok ? 'Playing on the camera.' : 'Could not play: ' + j.detail); });
    });
  });
})();
