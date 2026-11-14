// Togavsu Email taskpane. The device grant from the core (CORE-28) is kept in roaming
// settings and sent with every request; the module exchanges it for a 10-minute
// context token each time.
(function () {
  "use strict";
  var base = window.location.pathname.replace(/\/outlook-addin\/.*$/, "");
  var corePage = window.location.origin + "/modules/email/device-connect";

  function grant() { return Office.context.roamingSettings.get("togavsuGrant") || ""; }
  function api(method, path, body) {
    var opts = { method: method, headers: { "X-Togavsu-Device-Grant": grant() } };
    if (body) { opts.headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
    return fetch(base + "/addin/api/" + path, opts).then(function (r) {
      if (r.status === 401) { Office.context.roamingSettings.remove("togavsuGrant"); Office.context.roamingSettings.saveAsync(); show("signin"); throw new Error("signed out"); }
      if (!r.ok) { return r.json().then(function (p) { throw new Error(p.detail || r.statusText); }); }
      return r.json();
    });
  }
  function show(id) {
    ["signin", "read", "compose"].forEach(function (s) { document.getElementById(s).classList.toggle("hidden", s !== id); });
  }
  function li(list, text, onClick) {
    var el = document.createElement("li"); var b = document.createElement("button");
    b.textContent = text; b.addEventListener("click", onClick); el.appendChild(b); list.appendChild(el);
  }
  function signIn() {
    Office.context.ui.displayDialogAsync(corePage, { height: 60, width: 40 }, function (res) {
      if (res.status !== Office.AsyncResultStatus.Succeeded) { return; }
      var d = res.value;
      d.addEventHandler(Office.EventType.DialogMessageReceived, function (arg) {
        try { var g = JSON.parse(arg.message).grant; Office.context.roamingSettings.set("togavsuGrant", g); Office.context.roamingSettings.saveAsync(start); } catch (e) {}
        d.close();
      });
    });
  }
  function matterSearch(input, list, pick) {
    var t;
    input.addEventListener("input", function () {
      clearTimeout(t);
      t = setTimeout(function () {
        api("GET", "matters?q=" + encodeURIComponent(input.value)).then(function (res) {
          list.innerHTML = "";
          (res.data || []).forEach(function (m) { li(list, m.number + " " + m.name + " (" + m.status + ")", function () { pick(m); }); });
        }).catch(function () {});
      }, 250);
    });
  }
  function fileItem(m) {
    var status = document.getElementById("status");
    status.textContent = "Filing to " + m.number + "...";
    Office.context.mailbox.item.getAsFileAsync(function (res) {
      if (res.status !== Office.AsyncResultStatus.Succeeded) { status.textContent = "Outlook didn't hand over the message."; return; }
      api("POST", "file", { matter_id: m.id, raw_base64: res.value }).then(function () {
        status.textContent = "Filed to " + m.number + " " + m.name + ".";
      }).catch(function (e) { status.textContent = e.message; });
    });
  }
  function startRead() {
    show("read");
    var item = Office.context.mailbox.item;
    var addrs = function (l) { return (l || []).map(function (a) { return a.emailAddress; }); };
    api("POST", "suggest", { from: item.from ? item.from.emailAddress : "", to: addrs(item.to), cc: addrs(item.cc), subject: item.subject,
      internet_message_id: item.internetMessageId }).then(function (res) {
      var el = document.getElementById("suggest"); el.innerHTML = "";
      if (res.filed) { el.textContent = "Already filed to " + res.filed.number + " " + res.filed.name + "."; return; }
      var ul = document.createElement("ul"); el.appendChild(ul);
      if (!res.suggested && !(res.candidates || []).length) { el.textContent = "No matter found for these addresses."; return; }
      [res.suggested].concat(res.candidates || []).filter(Boolean).forEach(function (m) {
        li(ul, "File to " + m.number + " " + m.name, function () { fileItem(m); });
      });
    }).catch(function (e) { document.getElementById("suggest").textContent = e.message; });
    matterSearch(document.getElementById("matterQ"), document.getElementById("matters"), fileItem);
    var cq = document.getElementById("contactQ"), cl = document.getElementById("contacts"), t;
    cq.addEventListener("input", function () {
      clearTimeout(t);
      t = setTimeout(function () {
        api("GET", "contacts?q=" + encodeURIComponent(cq.value)).then(function (res) {
          cl.innerHTML = "";
          (res.data || []).forEach(function (c) { var el = document.createElement("li"); el.textContent = c.full_name; cl.appendChild(el); });
        }).catch(function () {});
      }, 250);
    });
  }
  function startCompose() {
    show("compose");
    matterSearch(document.getElementById("composeQ"), document.getElementById("composeMatters"), function (m) {
      var st = document.getElementById("composeStatus");
      api("GET", "plus-address?matter_id=" + encodeURIComponent(m.id)).then(function (res) {
        Office.context.mailbox.item.bcc.addAsync([res.address], function () { st.textContent = "The sent copy will be filed to " + m.number + "."; });
      }).catch(function (e) { st.textContent = e.message; });
    });
  }
  function start() {
    if (!grant()) { show("signin"); return; }
    api("GET", "ping").then(function () {
      if (Office.context.mailbox.item && Office.context.mailbox.item.itemId === undefined && Office.context.mailbox.item.bcc) { startCompose(); } else { startRead(); }
    }).catch(function () {});
  }
  Office.onReady(function () {
    document.getElementById("signinBtn").addEventListener("click", signIn);
    start();
  });
})();
