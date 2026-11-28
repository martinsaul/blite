// Hands a share link (https://HOST/j/CODE#password) to the installed app via
// the blite:// scheme, naming HOST as the room's server. The password stays in the fragment: it is never sent to
// the server and is removed from the address bar right away.
(function () {
  var code = document.body.dataset.code;
  var pw = location.hash.slice(1);
  var e2ee = new URLSearchParams(location.search).get("e2ee") === "1";
  history.replaceState(null, "", location.pathname + location.search);
  // The room is on this server: the app joins it here, whatever its own
  // default is. Older apps ignore unknown parameters. No password: an open room.
  var server = location.protocol === "http:" ? "ws://" + location.host : location.host;
  var target = "blite://join/" + encodeURIComponent(code) + "?server=" + encodeURIComponent(server) + (e2ee ? "&e2ee=1" : "") + (pw ? "#" + pw : "");
  // The desktop app's retry button, or (on Android) the Android app's
  // button: the Android app takes blite:// links too. Its https fallback
  // would only reload this page: Chrome keeps a link to the page it is on.
  ["retry", "android-open"].forEach(function (id) {
    var a = document.getElementById(id);
    if (a) a.href = target;
  });
  location.href = target;
  setTimeout(function () {
    document.getElementById("opening").hidden = true;
    document.getElementById("missing").hidden = false;
  }, 2000);
})();
