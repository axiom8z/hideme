#!/usr/bin/env python
"""Build small valid samples with real structure and fake metadata for the sweep.

Everything that would otherwise differ between runs, such as random IDs and the time of
day, is fixed, so the same pinned packages on the same platform give identical files.
The digest printed last is what to compare.

Usage: python mksamples.py [OUTDIR]
"""
import argparse, hashlib, io, os, re, struct, subprocess, zipfile, zlib

import imageio_ffmpeg
from PIL import Image, ImageCms, PngImagePlugin, TiffImagePlugin
import pillow_heif

HERE = os.path.dirname(os.path.abspath(__file__))
FF = imageio_ffmpeg.get_ffmpeg_exe()

XMP = (b'<?xpacket begin="\xef\xbb\xbf" id="W5M0MpCehiHzreSzNTczkc9d"?>'
       b'<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">'
       b'<rdf:Description rdf:about="" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:creator>Test Author</dc:creator>'
       b'</rdf:Description></rdf:RDF></x:xmpmeta><?xpacket end="w"?>')


def pic(w=16, h=16, seed=0, mode="RGB"):
    im = Image.new("RGB", (w, h))
    px = im.load()
    for y in range(h):
        for x in range(w):
            px[x, y] = ((x * 16 + seed * 40) % 256, (y * 16 + seed * 10) % 256, ((x ^ y) * 12 + seed) % 256)
    return im.convert(mode)


def exif_bytes():
    ex = Image.Exif()
    ex[0x010F] = "FakeMake"
    ex[0x0110] = "FakeModel"
    ex[0x0131] = "FakeSoftware"
    ex[0x013B] = "Test Author"
    ex[0x0112] = 1
    ifd = ex.get_ifd(0x8769)
    ifd[0x9003] = "2024:01:02 03:04:05"
    gps = ex.get_ifd(0x8825)
    gps[1] = "N"
    gps[2] = (48.0, 51.0, 29.0)
    gps[3] = "E"
    gps[4] = (2.0, 17.0, 40.0)
    return ex.tobytes()


def small_icc():
    profile = bytearray(ImageCms.ImageCmsProfile(ImageCms.createProfile("sRGB")).tobytes())
    profile[24:36] = struct.pack(">6H", 2024, 1, 2, 3, 4, 5)  # the time it was made
    profile[84:100] = bytes(16)  # its ID, a hash that the time feeds into
    return bytes(profile)


def png_chunk(t, d):
    return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xffffffff)


def add_png_chunks(png, chunks):
    """Insert chunks right after IHDR."""
    i = 8 + 4 + 4 + 13 + 4
    return png[:i] + b"".join(png_chunk(t, d) for t, d in chunks) + png[i:]


def gen_images(out):
    icc = small_icc()
    ex = exif_bytes()
    # JPEG
    im = pic(16, 16)
    im.save(f"{out}/jpeg_baseline.jpg", "JPEG", quality=70, exif=ex, comment=b"Test comment", xmp=XMP)
    im.save(f"{out}/jpeg_progressive.jpg", "JPEG", quality=70, progressive=True, exif=ex, comment=b"Test comment", xmp=XMP)
    im.save(f"{out}/jpeg_icc_dri.jpg", "JPEG", quality=70, restart_marker_blocks=1, comment=b"Test comment")
    # PNG
    meta = PngImagePlugin.PngInfo()
    meta.add_text("Author", "Test Author")
    meta.add_itxt("Comment", "Test comment", lang="en", tkey="Comment")
    meta.add_text("Description", "Test description zipped", zip=True)
    png = io.BytesIO()
    pic(8, 8).save(png, "PNG", pnginfo=meta, dpi=(72, 72), exif=ex)
    d = png.getvalue()
    d = add_png_chunks(d, [(b"gAMA", struct.pack(">I", 45455)), (b"sRGB", b"\x00"),
                           (b"tIME", struct.pack(">HBBBBB", 2024, 1, 2, 3, 4, 5))])
    open(f"{out}/png_rgb.png", "wb").write(d)
    png = io.BytesIO()
    pic(8, 8).convert("P", palette=Image.ADAPTIVE, colors=16).save(png, "PNG", transparency=0, pnginfo=meta)
    d = png.getvalue()
    d = add_png_chunks(d, [(b"tIME", struct.pack(">HBBBBB", 2024, 1, 2, 3, 4, 5))])
    open(f"{out}/png_palette.png", "wb").write(d)
    png = io.BytesIO()
    pic(8, 8).save(png, "PNG", icc_profile=icc, pnginfo=meta)
    open(f"{out}/png_icc.png", "wb").write(png.getvalue())
    # APNG
    frames = [pic(8, 8, s) for s in range(3)]
    png = io.BytesIO()
    frames[0].save(png, "PNG", save_all=True, append_images=frames[1:], duration=100, loop=0, default_image=False, pnginfo=meta)
    open(f"{out}/apng.png", "wb").write(png.getvalue())
    # WebP
    pic(16, 16).save(f"{out}/webp_lossy.webp", "WEBP", quality=50, exif=ex, xmp=XMP, icc_profile=b"")
    pic(16, 16).save(f"{out}/webp_lossless.webp", "WEBP", lossless=True, exif=ex, xmp=XMP)
    pic(16, 16).save(f"{out}/webp_simple.webp", "WEBP", quality=50)
    frames = [pic(16, 16, s) for s in range(3)]
    frames[0].save(f"{out}/webp_anim.webp", "WEBP", save_all=True, append_images=frames[1:], duration=100, loop=0, quality=50, exif=ex, xmp=XMP)
    # GIF
    frames = [pic(8, 8, s).convert("P", palette=Image.ADAPTIVE, colors=8) for s in range(3)]
    frames[0].save(f"{out}/gif_anim.gif", "GIF", save_all=True, append_images=frames[1:], duration=100, loop=0, comment=b"Test comment")
    frames[0].save(f"{out}/gif_still.gif", "GIF", comment=b"Test comment")
    # TIFF
    ifd = TiffImagePlugin.ImageFileDirectory_v2()
    ifd[0x013B] = "Test Author"
    ifd[0x0131] = "FakeSoftware"
    ifd[0x010F] = "FakeMake"
    ifd[0x0110] = "FakeModel"
    ifd[0x010E] = "Test description"
    ifd[0x0132] = "2024:01:02 03:04:05"
    pic(8, 8).save(f"{out}/tiff_raw.tif", "TIFF", tiffinfo=ifd, compression="raw")
    pic(8, 8).save(f"{out}/tiff_lzw.tif", "TIFF", tiffinfo=ifd, compression="tiff_lzw")
    pic(8, 8).save(f"{out}/tiff_deflate.tif", "TIFF", tiffinfo=ifd, compression="tiff_adobe_deflate")
    pic(8, 8).save(f"{out}/tiff_jpeg.tif", "TIFF", tiffinfo=ifd, compression="jpeg")
    frames = [pic(8, 8, s) for s in range(2)]
    frames[0].save(f"{out}/tiff_multi.tif", "TIFF", save_all=True, append_images=frames[1:], tiffinfo=ifd, compression="raw")
    # HEIC, AVIF
    pillow_heif.register_heif_opener()
    h = pillow_heif.from_pillow(pic(16, 16))
    h.save(f"{out}/heic.heic", quality=30, exif=ex, xmp=XMP)
    h.save(f"{out}/heic_icc.heic", quality=30, icc_profile=icc)
    frames = [pic(16, 16, s) for s in range(2)]
    h2 = pillow_heif.from_pillow(frames[0])
    h2.add_from_pillow(frames[1])
    h2.save(f"{out}/heic_multi.heic", quality=30, exif=ex)
    pic(16, 16).save(f"{out}/avif.avif", "AVIF", quality=30, exif=ex, xmp=XMP, speed=9)


def ff(*args):
    subprocess.run([FF, "-v", "error", "-y", "-nostdin", *args], check=True)


V = ["-f", "lavfi", "-i", "testsrc=size=16x16:rate=5:duration=0.6"]
A = ["-f", "lavfi", "-i", "sine=frequency=440:duration=0.3:sample_rate=8000"]
A44 = ["-f", "lavfi", "-i", "sine=frequency=440:duration=0.2:sample_rate=44100"]
MD = ["-metadata", "title=Test Title", "-metadata", "artist=Test Author", "-metadata", "comment=Test comment",
      "-metadata", "date=2024-01-02", "-metadata", "encoder=FakeEncoder"]
X264 = ["-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "2"]
# Matroska and Ogg muxers otherwise write random IDs.
FIXED = ["-fflags", "+bitexact"]


def gen_av(out):
    ff(*V, *A44, *X264, "-c:a", "aac", "-b:a", "16k", *MD, "-metadata", "location=+48.8584+002.2945/", "-movflags", "use_metadata_tags", f"{out}/mp4_mdta.mp4")
    ff(*V, *A44, *X264, "-c:a", "aac", "-b:a", "16k", *MD, f"{out}/mp4_plain.mp4")
    ff(*V, *A44, *X264, "-c:a", "aac", "-b:a", "16k", *MD, "-movflags", "faststart", f"{out}/mp4_faststart.mp4")
    ff(*V, *X264, *MD, "-movflags", "frag_keyframe+empty_moov+default_base_moof", f"{out}/mp4_frag.mp4")
    ff(*V, *X264, "-bf", "2", "-force_key_frames", "expr:1", "-write_tmcd", "1", "-c:a", "aac", *MD, f"{out}/mov_plain.mov")
    ff(*V, *A44, *X264, "-c:a", "pcm_s16le", *MD, f"{out}/mov_pcm.mov")
    ff(*A44, "-c:a", "aac", "-b:a", "16k", *MD, f"{out}/m4a.m4a")
    # ismv writes an mfra with tfra
    ff(*V, *X264, "-f", "ismv", f"{out}/mp4_ismv.mp4")
    # MKV. A small attachment and chapters come from an ffmetadata file.
    meta = f"{out}/_chap.txt"
    open(meta, "w").write(";FFMETADATA1\ntitle=Test Title\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=300\ntitle=Test chapter\n")
    att = f"{out}/_att.txt"
    open(att, "w").write("Test attachment")
    ff(*V, *A44, "-i", meta, "-map", "0", "-map", "1", "-map_metadata", "2", "-map_chapters", "2", *X264, "-c:a", "libvorbis", "-q:a", "0", *MD,
       "-attach", att, "-metadata:s:t", "mimetype=text/plain", *FIXED, f"{out}/mkv.mkv")
    ff(*V, *A44, *X264, "-c:a", "flac", *MD, *FIXED, f"{out}/mkv_flac.mkv")
    ff(*V, *A44, "-c:v", "libvpx", "-b:v", "20k", "-g", "2", "-c:a", "libopus", "-b:a", "16k", *MD, *FIXED, f"{out}/webm_vp8.webm")
    ff(*V, "-c:v", "libvpx-vp9", "-b:v", "20k", "-g", "2", "-speed", "8", *MD, *FIXED, f"{out}/webm_vp9.webm")
    # AVI
    ff(*V, *A, "-c:v", "mpeg4", "-q:v", "20", "-g", "2", "-c:a", "pcm_s16le", *MD, f"{out}/avi_mpeg4.avi")
    ff(*V, *A44, "-c:v", "mjpeg", "-q:v", "20", "-c:a", "libmp3lame", "-b:a", "32k", *MD, f"{out}/avi_mjpeg.avi")
    # Audio
    ff(*A44, "-c:a", "libmp3lame", "-b:a", "32k", *MD, "-write_id3v1", "1", f"{out}/mp3.mp3")
    ff(*A, "-c:a", "flac", "-compression_level", "0", *MD, f"{out}/flac.flac")
    ff(*A44, "-c:a", "libvorbis", "-q:a", "0", *MD, *FIXED, f"{out}/vorbis.ogg")
    ff(*A44, "-c:a", "libopus", "-b:a", "16k", *MD, *FIXED, f"{out}/opus.opus")
    ff(*A, "-c:a", "pcm_s16le", *MD, "-write_bext", "1", f"{out}/wav.wav")
    ff(*A, "-c:a", "pcm_s16be", *MD, "-write_id3v2", "1", f"{out}/aiff.aiff")
    os.remove(meta)
    os.remove(att)


def add_xmp(pdf):
    # pikepdf would otherwise stamp the packet with the time and its own version.
    with pdf.open_metadata(set_pikepdf_as_editor=False) as m:
        m["dc:creator"] = ["Test Author"]
        m["pdf:Producer"] = "FakeProducer"
        m["xmp:MetadataDate"] = "2024-01-02T03:04:05"


def gen_docs(out):
    from reportlab.pdfgen import canvas
    import pikepdf
    buf = io.BytesIO()
    c = canvas.Canvas(buf, pagesize=(200, 100), pageCompression=0, invariant=1)
    c.setAuthor("Test Author")
    c.setTitle("Test Title")
    c.setSubject("Test subject")
    c.setKeywords("test keyword")
    c.setCreator("FakeCreator")
    c.drawString(10, 50, "Visible text")
    c.showPage()
    c.drawString(10, 50, "Second page")
    c.showPage()
    c.save()
    open(f"{out}/pdf_reportlab.pdf", "wb").write(buf.getvalue())
    buf.seek(0)
    with pikepdf.open(buf) as p:
        add_xmp(p)
        p.save(f"{out}/pdf_objstm.pdf", object_stream_mode=pikepdf.ObjectStreamMode.generate, compress_streams=True, static_id=True)
    buf.seek(0)
    with pikepdf.open(buf) as p:
        add_xmp(p)
        p.save(f"{out}/pdf_plainxmp.pdf", object_stream_mode=pikepdf.ObjectStreamMode.disable, compress_streams=False, static_id=True)
    svg = b'''<?xml version="1.0" encoding="UTF-8"?>
<!-- Created with FakeTool by Test Author -->
<?fake-pi Test Author?>
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:cc="http://creativecommons.org/ns#" xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape" xmlns:sodipodi="http://sodipodi.sourceforge.net/DTD/sodipodi-0.dtd" width="16" height="16" viewBox="0 0 16 16" version="1.1" id="svg1" inkscape:version="1.0 (fake)" sodipodi:docname="C:\\Users\\Test\\drawing.svg" inkscape:export-filename="C:\\Users\\Test\\out.png">
  <title>Test Title</title>
  <desc>Test description</desc>
  <metadata id="m1"><rdf:RDF><cc:Work rdf:about=""><dc:creator><cc:Agent><dc:title>Test Author</dc:title></cc:Agent></dc:creator></cc:Work></rdf:RDF></metadata>
  <sodipodi:namedview id="base" inkscape:window-width="1920"/>
  <defs id="d1"><linearGradient id="g1"><stop offset="0" stop-color="#ff0000"/></linearGradient></defs>
  <g id="layer1" inkscape:label="Layer" inkscape:groupmode="layer">
    <rect x="1" y="1" width="10" height="10" fill="url(#g1)" id="r1"/>
    <!-- inner comment -->
    <text x="2" y="12" font-size="4" id="t1">Hi</text>
  </g>
</svg>
'''
    open(f"{out}/svg.svg", "wb").write(svg)


def fix_zip(path):
    """Writers stamp ZIP entries with the time of day, and some of their properties too."""
    with zipfile.ZipFile(path) as z:
        entries = [(i.filename, i.compress_type, z.read(i.filename)) for i in z.infolist()]
    with zipfile.ZipFile(path, "w") as z:
        for name, method, data in entries:
            info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            info.compress_type = method
            info.create_system = 0
            z.writestr(info, re.sub(rb"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z?", b"2024-01-02T03:04:05Z", data))


def gen_office(out):
    import docx, openpyxl, pptx
    from odf.opendocument import OpenDocumentText
    from odf.text import P
    from odf import meta as odfmeta, dc as odfdc
    d = docx.Document()
    d.add_paragraph("Visible paragraph")
    d.core_properties.author = "Test Author"
    d.core_properties.title = "Test Title"
    d.core_properties.comments = "Test comment"
    d.save(f"{out}/docx.docx")
    w = openpyxl.Workbook()
    w.active["A1"] = "Visible cell"
    w.active["B1"] = 42
    w.properties.creator = "Test Author"
    w.properties.title = "Test Title"
    w.save(f"{out}/xlsx.xlsx")
    p = pptx.Presentation()
    s = p.slides.add_slide(p.slide_layouts[5])
    s.shapes.title.text = "Visible title"
    p.core_properties.author = "Test Author"
    p.save(f"{out}/pptx.pptx")
    o = OpenDocumentText()
    o.text.addElement(P(text="Visible paragraph"))
    o.meta.addElement(odfdc.Creator(text="Test Author"))
    o.meta.addElement(odfdc.Title(text="Test Title"))
    o.save(f"{out}/odt.odt")
    for name in ("docx.docx", "xlsx.xlsx", "pptx.pptx", "odt.odt"):
        fix_zip(f"{out}/{name}")


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("out", nargs="?", default=os.path.join(HERE, "work", "samples"))
    out = ap.parse_args().out
    os.makedirs(out, exist_ok=True)
    gen_images(out)
    gen_av(out)
    gen_docs(out)
    gen_office(out)
    digest = hashlib.sha256()
    for n in sorted(os.listdir(out)):
        with open(os.path.join(out, n), "rb") as f:
            data = f.read()
        digest.update(n.encode() + b"\0" + data)
        print(f"{len(data):8d} {n}")
    print("sample set sha256:", digest.hexdigest())
