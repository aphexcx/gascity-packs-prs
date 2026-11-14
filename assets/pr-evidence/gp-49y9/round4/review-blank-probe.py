import ctypes as c
import ctypes.util
import json
import unicodedata as ud

cf = c.CDLL(ctypes.util.find_library("CoreFoundation"))
ct = c.CDLL(ctypes.util.find_library("CoreText"))
cf.CFStringCreateWithCString.argtypes = [c.c_void_p, c.c_char_p, c.c_uint32]
cf.CFStringCreateWithCString.restype = c.c_void_p
cf.CFRelease.argtypes = [c.c_void_p]
cf.CFStringGetCString.argtypes = [c.c_void_p, c.c_char_p, c.c_long, c.c_uint32]
cf.CFStringGetCString.restype = c.c_bool
ct.CTFontCreateWithName.argtypes = [c.c_void_p, c.c_double, c.c_void_p]
ct.CTFontCreateWithName.restype = c.c_void_p
ct.CTFontCopyPostScriptName.argtypes = [c.c_void_p]
ct.CTFontCopyPostScriptName.restype = c.c_void_p
ct.CTFontGetGlyphsForCharacters.argtypes = [c.c_void_p, c.POINTER(c.c_uint16), c.POINTER(c.c_uint16), c.c_long]
ct.CTFontGetGlyphsForCharacters.restype = c.c_bool

class Point(c.Structure):
    _fields_ = [("x", c.c_double), ("y", c.c_double)]

class Size(c.Structure):
    _fields_ = [("width", c.c_double), ("height", c.c_double)]

class Rect(c.Structure):
    _fields_ = [("origin", Point), ("size", Size)]

ct.CTFontGetBoundingRectsForGlyphs.argtypes = [c.c_void_p, c.c_uint32, c.POINTER(c.c_uint16), c.POINTER(Rect), c.c_long]
ct.CTFontGetBoundingRectsForGlyphs.restype = Rect

chars = (c.c_uint16 * 65536)(*range(65536))
for font_name in ["Helvetica", "Times", "Apple Symbols", "Arial Unicode MS", "Apple Braille"]:
    name = cf.CFStringCreateWithCString(None, font_name.encode(), 0x08000100)
    font = ct.CTFontCreateWithName(name, 20, None)
    cf.CFRelease(name)
    actual = ct.CTFontCopyPostScriptName(font)
    actual_buffer = c.create_string_buffer(256)
    cf.CFStringGetCString(actual, actual_buffer, 256, 0x08000100)
    cf.CFRelease(actual)
    glyphs = (c.c_uint16 * 65536)()
    ct.CTFontGetGlyphsForCharacters(font, chars, glyphs, 65536)
    rects = (Rect * 65536)()
    ct.CTFontGetBoundingRectsForGlyphs(font, 0, glyphs, rects, 65536)
    matches = []
    inspected = 0
    for r, (glyph, rect) in enumerate(zip(glyphs, rects)):
        cat = ud.category(chr(r))
        if not glyph or cat in ("Cc", "Cf", "Mn", "Me", "Cn", "Cs") or cat.startswith("Z"):
            continue
        if r in (0x115F, 0x1160, 0x3164, 0xFFA0, 0x2800, 0xFFFC):
            continue
        inspected += 1
        if rect.size.width == 0 and rect.size.height == 0:
            matches.append({"codepoint": f"U+{r:04X}", "name": ud.name(chr(r), "UNNAMED"), "category": cat, "glyph": glyph})
    print(json.dumps({"requested_font": font_name, "actual_font": actual_buffer.value.decode(), "range": "U+0000..U+FFFF", "present_candidate_glyphs": inspected, "outside_candidates": matches}))
    cf.CFRelease(font)
