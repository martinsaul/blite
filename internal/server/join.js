// Hands a share link (https://blite.../j/CODE#password) to the installed app via
// the blite:// scheme. The password stays in the fragment: it is never sent to
// the server and is removed from the address bar right away.
(function () {
  var code = document.body.dataset.code;
  var pw = location.hash.slice(1);
  history.replaceState(null, "", location.pathname);
  if (!pw) {
    document.getElementById("opening").hidden = true;
    document.getElementById("nopw").hidden = false;
    return;
  }
  var target = "blite://join/" + encodeURIComponent(code) + "#" + pw;
  var retry = document.getElementById("retry");
  retry.href = target;
  location.href = target;
  setTimeout(function () {
    document.getElementById("opening").hidden = true;
    document.getElementById("missing").hidden = false;
  }, 2000);
})();
