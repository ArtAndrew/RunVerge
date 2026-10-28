#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Render the seven README diagram languages. Standard library only."""
import copy
import json
from pathlib import Path
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parent
NS = 'http://www.w3.org/2000/svg'
ET.register_namespace('', NS)
LOCALES = ('en', 'ru', 'es', 'pt-BR', 'de', 'fr', 'zh-CN')


def layout(kind):
    if kind == 'workflow':
        sizes = {2:28,3:17,4:40,5:25,21:21,31:16}
        for indices,size in [(range(6,11),19),(range(11,16),26),(range(16,21),20),(range(22,25),24),(range(25,31),20)]:
            sizes.update(dict.fromkeys(indices,size))
        bounds = {2:(90,350),3:(930,1140),4:(48,1152),5:(48,1152),21:(48,1152),31:(48,1152)}
        for start in (6,11,16):
            bounds.update({start+i:(69+i*227,230+i*227) for i in range(5)})
        for i,x in enumerate((48,427,806)):
            bounds[22+i]=(x,x+330)
            bounds[25+i*2]=bounds[26+i*2]=(x,x+330)
        return sizes,bounds
    if kind == 'starting-points':
        sizes={2:34,3:22,4:17,5:29,14:17,15:29,16:22,21:21,22:19,23:19,24:17}
        sizes.update(dict.fromkeys(range(6,10),23));sizes.update(dict.fromkeys(range(10,14),20));sizes.update(dict.fromkeys(range(17,21),23))
        bounds={i:(42,1158) for i in (2,3,23,24)}
        bounds.update({i:(68,556) for i in range(4,14)})
        bounds.update({i:(636,1130) for i in (14,15,16,21,22)})
        bounds.update({17:(652,850),18:(900,1114),19:(652,850),20:(900,1114)})
        return sizes,bounds
    sizes={2:34,3:22,16:22,17:20,18:19}
    sizes.update(dict.fromkeys(range(4,7),17));sizes.update(dict.fromkeys(range(7,10),26));sizes.update(dict.fromkeys(range(10,16),20))
    bounds={i:(42,1158) for i in (2,3,18)}
    bounds.update({16:(65,1135),17:(65,1135)})
    for i,x in enumerate((65,448,831)):
        bounds[4+i]=bounds[7+i]=(x,x+304)
        bounds[10+i*2]=bounds[11+i*2]=(x,x+304)
    return sizes,bounds


def render():
    labels=json.loads((ROOT/'labels.json').read_text(encoding='utf-8'))
    overrides_path=ROOT/'font-sizes.json'
    overrides=json.loads(overrides_path.read_text()) if overrides_path.exists() else {}
    assert set(labels)==set(LOCALES)
    for kind,count in [('workflow',32),('starting-points',25),('execution-controls',19)]:
        template=ET.parse(ROOT/(kind+'.svg')).getroot()
        sizes,bounds=layout(kind)
        for locale in LOCALES:
            svg=copy.deepcopy(template)
            svg.set('lang',locale)
            nodes=[n for n in svg.iter() if n.tag.rsplit('}',1)[-1] in ('title','desc','text')]
            assert len(nodes)==len(labels[locale][kind])==count,(locale,kind)
            for index,(node,text) in enumerate(zip(nodes,labels[locale][kind])):
                assert isinstance(text,str) and text.strip(),(locale,kind,index)
                node.text=text
                if index in sizes:
                    size=overrides.get(locale,{}).get(kind,{}).get(str(index),sizes[index])
                    assert 16<=size<=48
                    node.set('font-size',str(size))
                    node.set('data-label',str(index))
                    node.set('data-left',str(bounds[index][0]));node.set('data-right',str(bounds[index][1]))
            output=ROOT if locale=='en' else ROOT/locale
            output.mkdir(exist_ok=True)
            ET.ElementTree(svg).write(output/(kind+'.svg'),encoding='utf-8',xml_declaration=False)
            with (output/(kind+'.svg')).open('a') as stream:stream.write('\n')


if __name__=='__main__':
    render()
