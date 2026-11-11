// Hands a share link (https://HOST/j/CODE#password) to the installed app via
// the blite:// scheme, naming HOST as the room's server. The password stays in the fragment: it is never sent to
// the server and is removed from the address bar right away.
(function () {
  var code = document.body.dataset.code;
  var pw = location.hash.slice(1);
  history.replaceState(null, "", location.pathname);
  // The room is on this server: the app joins it here, whatever its own
  // default is. Older apps ignore the parameter. No password: an open room.
  var server = location.protocol === "http:" ? "ws://" + location.host : location.host;
  var target = "blite://join/" + encodeURIComponent(code) + "?server=" + encodeURIComponent(server) + (pw ? "#" + pw : "");
  var retry = document.getElementById("retry");
  retry.href = target;
  location.href = target;
  setTimeout(function () {
    document.getElementById("opening").hidden = true;
    document.getElementById("missing").hidden = false;
  }, 2000);
})();
