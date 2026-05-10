// Signal analytics — single-file helper. Drop this into the target Flutter
// repo at lib/track.dart. Use the top-level `track(...)`, `setUserId(...)`,
// `setSessionId(...)`, and `initAnalytics(...)` functions directly — do not
// turn this into an SDK.
//
// Session ID strategy: this helper does NOT mint its own session id when
// the host app already has one. Pass the host's existing session id via
// `initAnalytics(sessionId: applicationVariables.sessionID)` so analytics
// events carry the same id that goes into HTTP headers (`X-Session-Id`,
// sent via lib/shared/utils/callAPI.dart) and the webview's
// `signal_session_id`. The helper falls back to its own UUID v4 only when
// no id is provided — that case should never happen in production.
//
// Wire endpoint + write key via --dart-define at build time:
//   flutter run \
//     --dart-define=ANALYTICS_ENDPOINT=https://collector.example.run.app \
//     --dart-define=ANALYTICS_WRITE_KEY=wk_dev_xxx \
//     --dart-define=APP_VERSION=1.0.0

import 'dart:async';
import 'dart:convert';
import 'dart:io' show Platform;

import 'package:flutter/foundation.dart';
import 'package:flutter/widgets.dart';
import 'package:hive_ce_flutter/hive_flutter.dart';
import 'package:http/http.dart' as http;
import 'package:uuid/uuid.dart';

const String _endpoint = String.fromEnvironment('ANALYTICS_ENDPOINT');
const String _writeKey = String.fromEnvironment('ANALYTICS_WRITE_KEY');
const String _appVersion =
    String.fromEnvironment('APP_VERSION', defaultValue: '0.0.0');

const Duration _flushInterval = Duration(seconds: 5);
const int _flushAt = 20;
const int _maxRequestEvents = 100;

class _Analytics with WidgetsBindingObserver {
  static final _Analytics instance = _Analytics._();
  _Analytics._();

  final Uuid _uuid = const Uuid();
  final List<Map<String, dynamic>> _queue = [];
  Box<dynamic>? _box;
  http.Client? _http;
  String _anonId = '';
  String _sessionId = '';
  String? _userId;
  bool _booted = false;
  bool _sending = false;

  Future<void> init({String? sessionId}) async {
    if (_booted) return;
    _booted = true;
    await Hive.initFlutter();
    _box = await Hive.openBox<dynamic>('signal');
    _http = http.Client();

    // Persistent per-device anon id. Survives app reinstalls only as long
    // as the encrypted Hive box persists (deleted with the app).
    _anonId = _box!.get('a_id') as String? ?? 'a_${_uuid.v4().replaceAll('-', '')}';
    await _box!.put('a_id', _anonId);

    // Session id: prefer what the host passed (e.g. applicationVariables.sessionID).
    // Fall back to minting one only if nothing was provided.
    if (sessionId != null && sessionId.isNotEmpty) {
      _sessionId = sessionId;
    } else {
      _sessionId = _uuid.v4();
      if (kDebugMode) {
        debugPrint('[track] no sessionId passed to initAnalytics; minted '
            'one locally. Pass the host app\'s session id (e.g. '
            'applicationVariables.sessionID) so events match HTTP / webview '
            'tracking.');
      }
    }

    WidgetsBinding.instance.addObserver(this);
    Timer.periodic(_flushInterval, (_) => unawaited(_flush()));
  }

  void setUserId(String? id) => _userId = id;

  // Update the session id at runtime (e.g. when the host app receives a
  // new session id from a downstream API or rotates after force-relaunch).
  void setSessionId(String sessionId) {
    if (sessionId.isNotEmpty) _sessionId = sessionId;
  }

  void track(String eventName, [Map<String, dynamic>? properties]) {
    if (!_booted || _box == null) return;
    if (_endpoint.isEmpty || _writeKey.isEmpty) return;
    final ev = {
      'event_id': _uuid.v4(),
      'event_name': eventName,
      'user_id': _userId,
      'anonymous_id': _anonId,
      'session_id': _sessionId,
      'client_ts': DateTime.now().toUtc().toIso8601String(),
      'properties': properties ?? <String, dynamic>{},
      'context': {
        'platform': Platform.isIOS ? 'flutter_ios' : 'flutter_android',
        'app_version': _appVersion,
        'sdk_version': '0.1.0',
      },
    };
    _queue.add(ev);
    if (_queue.length >= _flushAt) unawaited(_flush());
  }

  Future<void> reset() async {
    _userId = null;
    _anonId = 'a_${_uuid.v4().replaceAll('-', '')}';
    await _box?.put('a_id', _anonId);
    // Session id is host-managed; do NOT touch it here.
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == AppLifecycleState.paused ||
        state == AppLifecycleState.inactive ||
        state == AppLifecycleState.detached) {
      unawaited(_flush());
    }
  }

  Future<void> _flush() async {
    if (_queue.isEmpty || _sending || _http == null) return;
    if (_endpoint.isEmpty || _writeKey.isEmpty) {
      _queue.clear();
      return;
    }
    _sending = true;
    try {
      while (_queue.isNotEmpty) {
        final n = _queue.length < _maxRequestEvents ? _queue.length : _maxRequestEvents;
        final batch = _queue.sublist(0, n);
        try {
          final res = await _http!.post(
            Uri.parse('${_endpoint.replaceAll(RegExp(r'/+$'), '')}/v1/events'),
            headers: {
              'Content-Type': 'application/json',
              'X-Write-Key': _writeKey,
              'Idempotency-Key': _uuid.v4(),
            },
            body: jsonEncode({'batch': batch}),
          );
          if (res.statusCode >= 200 && res.statusCode < 300) {
            _queue.removeRange(0, n);
          } else if (res.statusCode >= 400 && res.statusCode < 500) {
            if (kDebugMode) {
              debugPrint('[track] dropping batch (${res.statusCode})');
            }
            _queue.removeRange(0, n);
          } else {
            return; // retry on next flush tick
          }
        } catch (_) {
          return; // network error → retry on next flush tick
        }
      }
    } finally {
      _sending = false;
    }
  }
}

Future<void> initAnalytics({String? sessionId}) =>
    _Analytics.instance.init(sessionId: sessionId);
void track(String eventName, [Map<String, dynamic>? properties]) =>
    _Analytics.instance.track(eventName, properties);
void setUserId(String? id) => _Analytics.instance.setUserId(id);
void setSessionId(String sessionId) => _Analytics.instance.setSessionId(sessionId);
Future<void> resetAnalytics() => _Analytics.instance.reset();

// Drop this into MaterialApp.navigatorObservers (or GoRouter.observers) to
// auto-fire page_viewed on every named route push/replace.
class TrackNavigatorObserver extends NavigatorObserver {
  @override
  void didPush(Route<dynamic> route, Route<dynamic>? previousRoute) {
    super.didPush(route, previousRoute);
    final name = route.settings.name;
    if (name != null && name.isNotEmpty) {
      track('page_viewed', {'name': name});
    }
  }

  @override
  void didReplace({Route<dynamic>? newRoute, Route<dynamic>? oldRoute}) {
    super.didReplace(newRoute: newRoute, oldRoute: oldRoute);
    final name = newRoute?.settings.name;
    if (name != null && name.isNotEmpty) {
      track('page_viewed', {'name': name});
    }
  }
}
