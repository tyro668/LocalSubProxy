package dev.avora.lmbp.spring;

import jakarta.servlet.http.HttpServletRequest;

/**
 * Tells the starter who the signed-in end user of the current request is (your login, session,
 * JWT, ...). Used by the {@link LmbpClient} methods without an explicit user id. Return
 * {@code null} when nobody is signed in. The id is opaque to LMBP (1-128 chars) and must be stable.
 */
@FunctionalInterface
public interface LmbpUserResolver {
  String resolve(HttpServletRequest request);
}
